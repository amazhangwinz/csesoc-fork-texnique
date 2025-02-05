package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

var (
	/**
	websocketUpgrader is used to upgrade incomming HTTP requests into a persistent websocket connection
	*/
	websocketUpgrader = websocket.Upgrader{
		// Apply the Origin Checker
		CheckOrigin:     nil,
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
)

var (
	ErrEventNotSupported = errors.New("this event type is not supported")
)

var handlers = map[string]EventHandler{
	EventStartGameOwner: StartGameHandler,
	EventGiveAnswer:     GiveAnswerHandler,
	EventRequestProblem: RequestProblemHandler,
}

type Problem struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Latex       string `json:"latex"`
}

func (p *Problem) CheckAnswer(submittedAnswer string) bool {
	return true // TODO: Implement this (check against answer)
}

type Problems struct {
	Problems []Problem `json:"problems"`
}

type User struct {
	password       string
	questionNumber int
	score          int
}

type GameState string

const (
	WaitingForPlayers GameState = "waiting"
	InPlay            GameState = "playing"
	Finished          GameState = "finished"
	DNE               GameState = "dne"
)

type Lobby struct {
	id        string
	name      string
	timeLimit int
	startTime *time.Time
	owner     *string
	gameState GameState

	// username to (hashed) password
	userMapping map[string]User
	// otp to username
	otpMapping map[string]string

	useCustom      bool
	CustomProblems []Problem
	CustomOrder    []int

	clients ClientList // TODO: investigate needs to be merged with userMapping (?)

	// Using a syncMutex here to be able to lcok state before editing clients
	// Could also use Channels to block
	sync.RWMutex

	// otps is a map of allowed OTP to accept connections from
	otps RetentionMap
}

// UUID to Lobby map
type LobbyList map[string]*Lobby

// Manager is used to hold references to all Clients Registered, and Broadcasting etc
type Manager struct {
	lobbies LobbyList
	ctx     context.Context
}

// NewManager is used to initalize all the values inside the manager
func NewManager(ctx context.Context) *Manager {
	m := &Manager{
		lobbies: make(LobbyList),
		ctx:     ctx,
	}
	return m
}

func NewLobby(ctx context.Context, name string, id string) *Lobby {
	l := &Lobby{
		userMapping:    make(map[string]User),
		otpMapping:     make(map[string]string),
		timeLimit:      600,
		id:             id,
		name:           name,
		owner:          nil,
		gameState:      WaitingForPlayers,
		startTime:      nil,
		clients:        make(ClientList),
		otps:           NewRetentionMap(ctx, 5*time.Second),
		CustomProblems: nil,
		CustomOrder:    nil,
	}

	return l
}

func (lobby *Lobby) startGame() {
	if lobby.gameState != WaitingForPlayers {
		panic("Game is already in progress")
	}
	lobby.gameState = InPlay
}

func (lobby *Lobby) endGame() {
	if lobby.gameState != InPlay {
		panic("Game isn't in progress")
	}
	lobby.gameState = Finished
}

func (lobby *Lobby) inPlay() bool {
	return lobby.gameState == InPlay
}

// routeEvent is used to make sure the correct event goes into the correct handler
func (m *Manager) routeEvent(event Event, c *Client) error {
	// Check if Handler is present in Map
	if handler, ok := handlers[event.Type]; ok {
		println(time.Now().Format("2006/01/02 15:04:05") +
			" Event from " + c.name + " in lobby " + c.lobby.name + ": " + event.Type,
		)
		// Execute the handler and return any err
		if err := handler(event, c); err != nil {
			return err
		}
		return nil
	} else {
		return ErrEventNotSupported
	}
}

// loginHandler is used to verify an user authentication and return a one time password
func (m *Manager) loginHandler(w http.ResponseWriter, r *http.Request) {

	type userLoginRequest struct {
		Username string `json:"username"`
		Password string `json:"password"`
		LobbyId  string `json:"lobbyId"` // UUID
	}

	var req userLoginRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	lobbyId := req.LobbyId
	lobby, lobbyExists := m.lobbies[lobbyId]
	if !lobbyExists {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Hashed password from the request
	hashedReqPassword, err := HashPassword(req.Password)
	if err != nil {
		log.Println(err)
		return
	}

	user, userExists := lobby.userMapping[req.Username]
	if !userExists {
		user.password = hashedReqPassword
		// Initialise user
		lobby.userMapping[req.Username] = user
	}

	// authenticate user / verify access token
	if CheckPasswordHash(req.Password, user.password) {
		// If authentication passes, set the owner of the lobby
		if lobby.owner == nil {
			lobby.owner = &req.Username
		}

		// add a new OTP
		otp := lobby.otps.NewOTP()
		lobby.otpMapping[otp.Key] = req.Username

		// format to return otp in to the frontend
		type response struct {
			OTP   string `json:"otp"`
			Lobby string `json:"lobby"`
		}
		resp := response{
			OTP:   otp.Key,
			Lobby: lobbyId,
		}

		data, err := json.Marshal(resp)
		if err != nil {
			log.Println(err)
			return
		}
		// return a response to the authenticated user with the OTP
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}

	// failure to auth
	w.WriteHeader(http.StatusUnauthorized)
}

// serveWS is a HTTP Handler that the has the Manager that allows connections
func (m *Manager) serveWS(w http.ResponseWriter, r *http.Request) {

	// Grab the OTP in the Get param
	otp := r.URL.Query().Get("otp")
	if otp == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	lobbyName := r.URL.Query().Get("l")
	lobby, lobbyExists := m.lobbies[lobbyName]
	if !lobbyExists {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if lobby.gameState == Finished {
		// Don't allow users to connected if the game has ended
		w.WriteHeader(http.StatusGone)
		return
	}

	// Verify OTP is existing
	if !lobby.otps.VerifyOTP(otp) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	log.Println("New connection")
	// Begin by upgrading the HTTP request
	conn, err := websocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	// Create New Client
	client := NewClient(conn, m, lobby, otp)
	// Add the newly created client to the manager
	lobby.addClient(client)

	go client.readMessages()
	go client.writeMessages()

	if lobby.gameState == WaitingForPlayers {
		// Sending newMember events to all joined clients
		var broadMessage = NewMemberEvent{client.name}

		data, err := json.Marshal(broadMessage)
		if err != nil {
			log.Println(err)
			return
		}

		var outgoingEvent = Event{EventNewMember, data}
		for c := range client.lobby.clients {
			if c.name != client.name {
				c.egress <- outgoingEvent
			}
			var smallMessage = NewMemberEvent{c.name}
			data, err = json.Marshal(smallMessage)
			if err != nil {
				log.Println(err)
				return
			}
			var smallOutgoingEvent = Event{EventNewMember, data}
			client.egress <- smallOutgoingEvent
		}
	} else if lobby.gameState == InPlay {
		var startGameMessage = StartGameEvent{*lobby.startTime, lobby.timeLimit}

		data, err := json.Marshal(startGameMessage)
		if err != nil {
			log.Println(err)
			return
		}

		var outgoingEvent = Event{EventStartGame, data}
		client.egress <- outgoingEvent

		newProblemMessage := client.getNewProblem()

		data, err = json.Marshal(newProblemMessage)
		if err != nil {
			log.Println(err)
			return
		}

		outgoingEvent = Event{EventNewProblem, data}
		client.egress <- outgoingEvent
	}
}

func (m *Manager) lobbyStatus(w http.ResponseWriter, r *http.Request) {
	type lobbyStatusRequest struct {
		Id string `json:"lobbyId"`
	}
	var req lobbyStatusRequest

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	type response struct {
		Status GameState `json:"lobbyStatus"`
	}

	lobby, lobbyExists := m.lobbies[req.Id]

	if !lobbyExists {
		var resp response
		// If lobby doesn't exist in map, either it's been deleted or the game has ended
		logFilepath := filepath.Join(".", "logs", req.Id+".result.json")
		if _, err := os.Stat(logFilepath); errors.Is(err, os.ErrNotExist) {
			resp = response{Status: DNE}
		} else {
			resp = response{Status: Finished}
		}
		data, err := json.Marshal(resp)

		if err != nil {
			log.Println(err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}

	resp := response{Status: lobby.gameState}
	data, err := json.Marshal(resp)
	if err != nil {
		log.Println(err)
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (m *Manager) createLobbyHandler(w http.ResponseWriter, r *http.Request) {
	type createLobbyRequest struct {
		Name string `json:"lobbyName"`
	}
	var req createLobbyRequest

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id := uuid.New().String()
	m.lobbies[id] = NewLobby(m.ctx, req.Name, id)

	// format to return otp in to the frontend
	type response struct {
		LobbyId string `json:"l"`
	}
	resp := response{
		LobbyId: id,
	}

	data, err := json.Marshal(resp)
	if err != nil {
		log.Println(err)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// TODO(madhav): need update these functions?
// addClient will add clients to our clientList
func (m *Lobby) addClient(client *Client) bool {
	// Lock so we can manipulate
	m.Lock()
	defer m.Unlock()

	// Add Client
	m.clients[client] = true
	return true
}

// removeClient will remove the client and clean up
func (m *Lobby) removeClient(client *Client) {
	m.Lock()
	defer m.Unlock()

	// Check if Client exists, then delete it
	if _, ok := m.clients[client]; ok {
		// close connection
		client.connection.Close()
		// remove
		delete(m.clients, client)
	}
}
