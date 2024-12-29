package lobbyserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	retryablehttp "github.com/hashicorp/go-retryablehttp"
	gameserver "github.com/simple64/simple64-netplay-server/internal/gameServer"
	"golang.org/x/net/websocket"
)

const (
	Accepted        = 0
	BadPassword     = 1
	MismatchVersion = 2
	RoomFull        = 3
	DuplicateName   = 4
	RoomDeleted     = 5
	BadName         = 6
	BadEmulator     = 7
	BadAuth         = 8
	Other           = 9
)

const (
	TypeRequestPlayers     = "request_players"
	TypeReplyPlayers       = "reply_players"
	TypeRequestGetRooms    = "request_get_rooms"
	TypeReplyGetRooms      = "reply_get_rooms"
	TypeRequestCreateRoom  = "request_create_room"
	TypeReplyCreateRoom    = "reply_create_room"
	TypeRequestJoinRoom    = "request_join_room"
	TypeReplyJoinRoom      = "reply_join_room"
	TypeRequestChatMessage = "request_chat_message"
	TypeReplyChatMessage   = "reply_chat_message"
	TypeRequestBeginGame   = "request_begin_game"
	TypeReplyBeginGame     = "reply_begin_game"
	TypeRequestMotd        = "request_motd"
	TypeReplyMotd          = "reply_motd"
	TypeRequestVersion     = "request_version"
	TypeReplyVersion       = "reply_version"
)

type LobbyServer struct {
	GameServers      map[string]*gameserver.GameServer
	Logger           logr.Logger
	Name             string
	Motd             string
	BasePort         int
	MaxGames         int
	DisableBroadcast bool
	EnableAuth       bool
}

type RoomData struct {
	Features  map[string]string `json:"features"`
	GameName  string            `json:"game_name"`
	Protected bool              `json:"protected"`
	Password  string            `json:"password,omitempty"`
	RoomName  string            `json:"room_name"`
	MD5       string            `json:"MD5"`
	Port      int               `json:"port"`
}

type SocketMessage struct {
	Message        string     `json:"message,omitempty"`
	ClientSha      string     `json:"client_sha,omitempty"`
	Emulator       string     `json:"emulator,omitempty"`
	PlayerName     string     `json:"player_name,omitempty"`
	AuthTime       string     `json:"authTime,omitempty"`
	Type           string     `json:"type"`
	Auth           string     `json:"auth,omitempty"`
	PlayerNames    []string   `json:"player_names,omitempty"`
	Room           *RoomData  `json:"room,omitempty"`
	Rooms          []RoomData `json:"rooms,omitempty"`
	Accept         int        `json:"accept"`
	NetplayVersion int        `json:"netplay_version,omitempty"`
}

const NetplayAPIVersion = 17

func (s *LobbyServer) sendData(ws *websocket.Conn, message SocketMessage) error {
	// s.Logger.Info("sending message", "message", message, "address", ws.Request().RemoteAddr)
	err := websocket.JSON.Send(ws, message)
	if err != nil {
		return fmt.Errorf("error sending data: %s", err.Error())
	}
	return nil
}

// this function finds the GameServer pointer based on the port number.
func (s *LobbyServer) findGameServer(port int) (string, *gameserver.GameServer) {
	for i, v := range s.GameServers {
		if v.Port == port {
			return i, v
		}
	}
	return "", nil
}

func (s *LobbyServer) updatePlayers(g *gameserver.GameServer) {
	if g == nil {
		return
	}
	var sendMessage SocketMessage
	sendMessage.PlayerNames = make([]string, 4) //nolint:gomnd,mnd
	sendMessage.Type = TypeReplyPlayers
	for i, v := range g.Players {
		sendMessage.PlayerNames[v.Number] = i
	}

	// send the updated player list to all connected players
	for _, v := range g.Players {
		if err := s.sendData(v.Socket, sendMessage); err != nil {
			s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", v.Socket.Request().RemoteAddr)
		}
	}
}

func (s *LobbyServer) publishDiscord(message string, channel string) {
	body := map[string]string{
		"content": message,
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		s.Logger.Error(err, "could not read body")
		return
	}
	httpClient := retryablehttp.NewClient()
	httpClient.Logger = nil
	httpRequest, err := retryablehttp.NewRequest(http.MethodPost, channel, bodyJSON)
	if err != nil {
		s.Logger.Error(err, "could not create request")
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("User-Agent", "simple64Bot (simple64.github.io, 1)")
	resp, err := httpClient.Do(httpRequest)
	if err != nil {
		s.Logger.Error(err, "could not send request")
	} else {
		resp.Body.Close()
	}
}

func (s *LobbyServer) announceDiscord(g *gameserver.GameServer) {
	roomType := "public"
	if g.Password != "" {
		roomType = "private"
	}

	message := fmt.Sprintf("New %s netplay room running in %s has been created! Come play %s", roomType, s.Name, g.GameName)

	if roomType == "public" {
		for i := range 10 {
			channel := os.Getenv(fmt.Sprintf("%s_CHANNEL_%d", strings.ToUpper(g.Emulator), i))
			if channel != "" {
				s.publishDiscord(message, channel)
			}
		}
	}

	devChannel := os.Getenv(fmt.Sprintf("%s_DEV_CHANNEL", strings.ToUpper(g.Emulator)))
	if devChannel != "" {
		s.publishDiscord(message, devChannel)
	}
}

func (s *LobbyServer) watchGameServer(name string, g *gameserver.GameServer) {
	go g.ManageBuffer()
	go g.ManagePlayers()
	for {
		if !g.Running {
			g.Logger.Info("game server deleted", "port", g.Port)
			delete(s.GameServers, name)
			return
		}
		time.Sleep(time.Second * 5) //nolint:gomnd,mnd
	}
}

func (s *LobbyServer) validateAuth(receivedMessage SocketMessage) bool {
	if !s.EnableAuth {
		return true
	}

	now := time.Now().UTC()
	timeAsInt, err := strconv.ParseInt(receivedMessage.AuthTime, 10, 64)
	if err != nil {
		s.Logger.Error(err, "could not parse time", "emulator", receivedMessage.Emulator)
		return false
	}
	receivedTime := time.UnixMilli(timeAsInt).UTC()

	timeDifference := now.Sub(receivedTime)
	absTimeDifference := time.Duration(math.Abs(float64(timeDifference)))
	maxAllowableDifference := 15 * time.Minute //nolint:gomnd,mnd

	if absTimeDifference > maxAllowableDifference {
		s.Logger.Error(fmt.Errorf("clock skew"), "bad time in auth request", "serverTime", now, "clientTime", receivedTime, "emulator", receivedMessage.Emulator)
		return false
	}

	h := sha256.New()
	h.Write([]byte(receivedMessage.AuthTime))

	authCode := os.Getenv(fmt.Sprintf("%s_AUTH", strings.ToUpper(receivedMessage.Emulator)))
	if authCode == "" {
		return false
	}
	h.Write([]byte(authCode))

	return receivedMessage.Auth == hex.EncodeToString(h.Sum(nil))
}

func (s *LobbyServer) wsHandler(ws *websocket.Conn) {
	authenticated := false
	defer ws.Close()

	// s.Logger.Info("new WS connection", "address", ws.Request().RemoteAddr)

	for {
		var receivedMessage SocketMessage
		err := websocket.JSON.Receive(ws, &receivedMessage)
		if err != nil {
			if errors.Is(err, io.EOF) {
				for i, v := range s.GameServers {
					if !v.Running {
						for k, w := range v.Players {
							if w.Socket == ws {
								v.Logger.Info("Player has left lobby", "player", k, "address", ws.Request().RemoteAddr)

								v.PlayersMutex.Lock() // any player can modify this, which would be in a different thread
								delete(v.Players, k)
								v.PlayersMutex.Unlock()

								s.updatePlayers(v)
							}
						}
						if len(v.Players) == 0 {
							v.Logger.Info("No more players in lobby, deleting")
							v.CloseServers()
							delete(s.GameServers, i)
						}
					}
				}
				// s.Logger.Info("closed WS connection", "address", ws.Request().RemoteAddr)
				return
			}
			s.Logger.Info("could not read WS message", "reason", err.Error(), "address", ws.Request().RemoteAddr)
			continue
		}

		// s.Logger.Info("received message", "message", receivedMessage)

		var sendMessage SocketMessage

		if receivedMessage.Type == TypeRequestCreateRoom {
			sendMessage.Type = TypeReplyCreateRoom
			_, exists := s.GameServers[receivedMessage.Room.RoomName]
			if exists {
				sendMessage.Accept = DuplicateName
				sendMessage.Message = "Room with this name already exists"
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			} else if receivedMessage.NetplayVersion != NetplayAPIVersion {
				sendMessage.Accept = MismatchVersion
				sendMessage.Message = "Client and server not at same API version. Please update your emulator"
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			} else if receivedMessage.Room.RoomName == "" {
				sendMessage.Accept = BadName
				sendMessage.Message = "Room name cannot be empty"
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			} else if receivedMessage.PlayerName == "" {
				sendMessage.Accept = BadName
				sendMessage.Message = "Player name cannot be empty"
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			} else if receivedMessage.Emulator == "" {
				sendMessage.Accept = BadEmulator
				sendMessage.Message = "Emulator name cannot be empty"
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			} else if !s.validateAuth(receivedMessage) {
				sendMessage.Accept = BadAuth
				sendMessage.Message = "Bad authentication code"
				s.Logger.Info("bad auth code", "message", receivedMessage, "address", ws.Request().RemoteAddr)
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			} else {
				var sendRoom RoomData
				sendMessage.Room = &sendRoom
				authenticated = true
				g := gameserver.GameServer{}
				sendMessage.Room.Port = g.CreateNetworkServers(s.BasePort, s.MaxGames, receivedMessage.Room.RoomName, receivedMessage.Room.GameName, receivedMessage.Emulator, s.Logger)
				if sendMessage.Room.Port == 0 {
					sendMessage.Accept = Other
					sendMessage.Message = "Failed to create room"
					if err := s.sendData(ws, sendMessage); err != nil {
						s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
					}
				} else {
					g.GameName = receivedMessage.Room.GameName
					g.MD5 = receivedMessage.Room.MD5
					g.ClientSha = receivedMessage.ClientSha
					g.Password = receivedMessage.Room.Password
					g.Emulator = receivedMessage.Emulator
					g.Players = make(map[string]gameserver.Client)
					g.Features = receivedMessage.Room.Features
					ip, _, err := net.SplitHostPort(ws.Request().RemoteAddr)
					if err != nil {
						g.Logger.Error(err, "could not parse IP", "IP", ws.Request().RemoteAddr)
					}
					g.Players[receivedMessage.PlayerName] = gameserver.Client{
						IP:     ip,
						Number: 0,
						Socket: ws,
					}
					s.GameServers[receivedMessage.Room.RoomName] = &g
					g.Logger.Info("Created new room", "port", g.Port, "creator", receivedMessage.PlayerName, "clientSHA", receivedMessage.ClientSha, "creatorIP", ws.Request().RemoteAddr, "features", receivedMessage.Room.Features)
					sendMessage.Accept = Accepted
					sendMessage.Room.RoomName = receivedMessage.Room.RoomName
					sendMessage.Room.GameName = receivedMessage.Room.GameName
					sendMessage.Room.MD5 = receivedMessage.Room.MD5
					sendMessage.Room.Protected = receivedMessage.Room.Password != ""
					sendMessage.PlayerName = receivedMessage.PlayerName
					sendMessage.Room.Features = receivedMessage.Room.Features
					if err := s.sendData(ws, sendMessage); err != nil {
						s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
					}
					s.announceDiscord(&g)
				}
			}
		} else if receivedMessage.Type == TypeRequestGetRooms {
			sendMessage.Type = TypeReplyGetRooms
			if receivedMessage.NetplayVersion != NetplayAPIVersion {
				sendMessage.Accept = MismatchVersion
				sendMessage.Message = "Client and server not at same API version. Please update your emulator"
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			} else if receivedMessage.Emulator == "" {
				sendMessage.Accept = BadEmulator
				sendMessage.Message = "Emulator name cannot be empty"
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			} else if !s.validateAuth(receivedMessage) {
				sendMessage.Accept = BadAuth
				sendMessage.Message = "Bad authentication code"
				s.Logger.Info("bad auth code", "message", receivedMessage, "address", ws.Request().RemoteAddr)
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			} else {
				authenticated = true
				var rooms []RoomData
				for i, v := range s.GameServers {
					if v.Running {
						continue
					}
					if receivedMessage.Emulator != v.Emulator {
						// room belongs to a different emulator
						continue
					}
					var room RoomData
					if v.Password == "" {
						room.Protected = false
					} else {
						room.Protected = true
					}
					room.RoomName = i
					room.MD5 = v.MD5
					room.Port = v.Port
					room.GameName = v.GameName
					room.Features = v.Features
					rooms = append(rooms, room)
				}
				sendMessage.Accept = Accepted
				sendMessage.Rooms = rooms
				if err := s.sendData(ws, sendMessage); err != nil {
					s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
				}
			}
		} else if receivedMessage.Type == TypeRequestJoinRoom {
			if !authenticated {
				s.Logger.Error(fmt.Errorf("bad auth"), "User tried to join room without being authenticated", "address", ws.Request().RemoteAddr)
				continue
			}
			var duplicateName bool
			var accepted int
			var message string
			sendMessage.Type = TypeReplyJoinRoom
			roomName, g := s.findGameServer(receivedMessage.Room.Port)
			if g != nil {
				for i := range g.Players {
					if receivedMessage.PlayerName == i {
						duplicateName = true
					}
				}
				if g.Password != "" && g.Password != receivedMessage.Room.Password {
					accepted = BadPassword
					message = "Incorrect password"
				} else if g.ClientSha != receivedMessage.ClientSha {
					accepted = MismatchVersion
					message = "Client versions do not match"
				} else if g.MD5 != receivedMessage.Room.MD5 {
					accepted = MismatchVersion
					message = "ROM does not match room ROM"
				} else if len(g.Players) >= 4 { //nolint:gomnd,mnd
					accepted = RoomFull
					message = "Room is full"
				} else if receivedMessage.PlayerName == "" {
					accepted = BadName
					message = "Player name cannot be empty"
				} else if duplicateName {
					accepted = DuplicateName
					message = "Player name already in use"
				} else {
					var number int
					var goodNumber bool
					for number = range 4 {
						goodNumber = true
						for _, v := range g.Players {
							if v.Number == number {
								goodNumber = false
							}
						}
						if goodNumber {
							break
						}
					}

					ip, _, err := net.SplitHostPort(ws.Request().RemoteAddr)
					if err != nil {
						g.Logger.Error(err, "could not parse IP", "IP", ws.Request().RemoteAddr)
					}
					g.PlayersMutex.Lock() // any player can modify this from their own thread
					g.Players[receivedMessage.PlayerName] = gameserver.Client{
						IP:     ip,
						Socket: ws,
						Number: number,
					}
					g.PlayersMutex.Unlock()

					g.Logger.Info("new player joining room", "player", receivedMessage.PlayerName, "playerIP", ws.Request().RemoteAddr, "number", number)
					var sendRoom RoomData
					sendMessage.Room = &sendRoom
					sendMessage.Room.RoomName = roomName
					sendMessage.Room.GameName = g.GameName
					sendMessage.Room.MD5 = g.MD5
					sendMessage.Room.Protected = g.Password != ""
					sendMessage.PlayerName = receivedMessage.PlayerName
					sendMessage.Room.Features = g.Features
					sendMessage.Room.Port = g.Port
				}
			} else {
				accepted = RoomDeleted
				message = "room has been deleted"
				s.Logger.Info("server not found (room deleted)", "message", receivedMessage, "address", ws.Request().RemoteAddr)
			}
			sendMessage.Accept = accepted
			sendMessage.Message = message
			if err := s.sendData(ws, sendMessage); err != nil {
				s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
			}
		} else if receivedMessage.Type == TypeRequestPlayers {
			if !authenticated {
				s.Logger.Error(fmt.Errorf("bad auth"), "User tried to request players without being authenticated", "address", ws.Request().RemoteAddr)
				continue
			}
			_, g := s.findGameServer(receivedMessage.Room.Port)
			if g != nil {
				s.updatePlayers(g)
			} else {
				s.Logger.Error(fmt.Errorf("could not find game server"), "server not found", "message", receivedMessage, "address", ws.Request().RemoteAddr)
			}
		} else if receivedMessage.Type == TypeRequestChatMessage {
			if !authenticated {
				s.Logger.Error(fmt.Errorf("bad auth"), "User tried to send a chat message without being authenticated", "address", ws.Request().RemoteAddr)
				continue
			}
			sendMessage.Type = TypeReplyChatMessage
			sendMessage.Message = fmt.Sprintf("%s: %s", receivedMessage.PlayerName, receivedMessage.Message)
			_, g := s.findGameServer(receivedMessage.Room.Port)
			if g != nil {
				for _, v := range g.Players {
					if err := s.sendData(v.Socket, sendMessage); err != nil {
						s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
					}
				}
			} else {
				s.Logger.Error(fmt.Errorf("could not find game server"), "server not found", "message", receivedMessage, "address", ws.Request().RemoteAddr)
			}
		} else if receivedMessage.Type == TypeRequestBeginGame {
			if !authenticated {
				s.Logger.Error(fmt.Errorf("bad auth"), "User tried to begin game without being authenticated", "address", ws.Request().RemoteAddr)
				continue
			}
			sendMessage.Type = TypeReplyBeginGame
			roomName, g := s.findGameServer(receivedMessage.Room.Port)
			if g != nil {
				if g.Running {
					g.Logger.Error(fmt.Errorf("game already running"), "game running", "message", receivedMessage, "address", ws.Request().RemoteAddr)
				} else {
					g.Running = true
					g.StartTime = time.Now()
					g.Logger.Info("starting game", "time", g.StartTime.Format(time.RFC3339))
					go s.watchGameServer(roomName, g)
					for _, v := range g.Players {
						if err := s.sendData(v.Socket, sendMessage); err != nil {
							s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
						}
					}
				}
			} else {
				s.Logger.Error(fmt.Errorf("could not find game server"), "server not found", "message", receivedMessage, "address", ws.Request().RemoteAddr)
			}
		} else if receivedMessage.Type == TypeRequestMotd {
			if !authenticated {
				s.Logger.Error(fmt.Errorf("bad auth"), "User tried to request the motd without being authenticated", "address", ws.Request().RemoteAddr)
				continue
			}
			sendMessage.Type = TypeReplyMotd
			sendMessage.Message = s.Motd
			if err := s.sendData(ws, sendMessage); err != nil {
				s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
			}
		} else if receivedMessage.Type == TypeRequestVersion {
			sendMessage.Type = TypeReplyVersion
			sendMessage.Message = getVersion()
			if err := s.sendData(ws, sendMessage); err != nil {
				s.Logger.Error(err, "failed to send message", "message", sendMessage, "address", ws.Request().RemoteAddr)
			}
		} else {
			s.Logger.Info("not a valid lobby message type", "message", receivedMessage, "address", ws.Request().RemoteAddr)
		}
	}
}

// this function figures out what is our outgoing IP address.
func (s *LobbyServer) getOutboundIP(dest *net.UDPAddr) (net.IP, error) {
	conn, err := net.DialUDP("udp4", nil, dest)
	if err != nil {
		return nil, fmt.Errorf("error creating udp %s", err.Error())
	}
	defer conn.Close()
	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("failed to parse address")
	}

	return localAddr.IP, nil
}

func (s *LobbyServer) processBroadcast(udpServer *net.UDPConn, addr *net.UDPAddr, buf []byte) {
	if buf[0] == 1 {
		s.Logger.Info(fmt.Sprintf("received broadcast from %s on %s", addr.String(), udpServer.LocalAddr().String()))
		// send back the address of the WebSocket server
		outboundIP, err := s.getOutboundIP(addr)
		if err != nil {
			s.Logger.Error(err, "could not get outbound IP")
			return
		}
		response := map[string]string{
			s.Name: fmt.Sprintf("ws://%s", net.JoinHostPort(outboundIP.String(), strconv.Itoa(s.BasePort))),
		}
		jsonData, err := json.Marshal(response)
		if err != nil {
			s.Logger.Error(err, "could not encode json data")
			return
		}
		_, err = udpServer.WriteTo(jsonData, addr)
		if err != nil {
			s.Logger.Error(err, "could not reply to broadcast")
			return
		}
		s.Logger.Info("responded to broadcast", "response", response)
	}
}

func (s *LobbyServer) runBroadcastServer(broadcastPort int) {
	broadcastServer, err := net.ListenUDP("udp4", &net.UDPAddr{Port: broadcastPort})
	if err != nil {
		s.Logger.Error(err, "could not listen for broadcasts")
		return
	}
	defer broadcastServer.Close()

	s.Logger.Info("listening for broadcasts")
	for {
		buf := make([]byte, 1500) //nolint:gomnd,mnd
		_, addr, err := broadcastServer.ReadFromUDP(buf)
		if err != nil {
			s.Logger.Error(err, "error reading broadcast packet")
			continue
		}
		s.processBroadcast(broadcastServer, addr, buf)
	}
}

func (s *LobbyServer) RunSocketServer(broadcastPort int) error {
	s.GameServers = make(map[string]*gameserver.GameServer)
	if !s.DisableBroadcast {
		go s.runBroadcastServer(broadcastPort)
	}

	server := websocket.Server{
		Handler:   s.wsHandler,
		Handshake: nil,
	}
	http.Handle("/", server)
	listenAddress := fmt.Sprintf(":%d", s.BasePort)

	s.Logger.Info("server running", "address", listenAddress, "version", getVersion(), "platform", runtime.GOOS, "arch", runtime.GOARCH, "goversion", runtime.Version(), "enable-auth", s.EnableAuth)

	err := http.ListenAndServe(listenAddress, nil) //nolint:gosec
	if err != nil {
		return fmt.Errorf("error listening on http port %s", err.Error())
	}
	return nil
}

func (s *LobbyServer) LogServerStats() {
	for {
		memStats := runtime.MemStats{}
		runtime.ReadMemStats(&memStats)
		s.Logger.Info("server stats", "games", len(s.GameServers), "NumGoroutine", runtime.NumGoroutine(), "HeapAlloc", memStats.HeapAlloc, "HeapObjects", memStats.HeapObjects)
		time.Sleep(time.Minute)
	}
}

func getVersion() string {
	version := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				version = setting.Value
			}
		}
	}
	return fmt.Sprintf("git: %s. api: %d", version, NetplayAPIVersion)
}
