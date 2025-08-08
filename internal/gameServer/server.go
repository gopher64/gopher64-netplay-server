package gameserver

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
)

type Client struct {
	Socket  *websocket.Conn
	IP      net.IP
	Number  int
	InLobby bool
}

type Registration struct {
	RegID  uint32
	Plugin byte
	Raw    byte
}

type GameServer struct {
	StartTime          time.Time
	Players            map[string]Client
	PlayersMutex       sync.Mutex
	TCPListener        *net.TCPListener
	UDPListener        *net.UDPConn
	Registrations      map[byte]*Registration
	RegistrationsMutex sync.Mutex
	TCPMutex           sync.Mutex
	TCPFiles           map[string][]byte
	CustomData         map[byte][]byte
	Logger             logr.Logger
	GameName           string
	Password           string
	ClientSha          string
	MD5                string
	Emulator           string
	TCPSettings        []byte
	GameData           GameData
	GameDataMutex      sync.Mutex
	Port               int
	HasSettings        bool
	Running            bool
	Features           map[string]string
	NeedsUpdatePlayers bool
	NumberOfPlayers    int
	BufferTarget       uint32
	QuitChannel        *chan bool
}

func (g *GameServer) CreateNetworkServers(basePort int, maxGames int, roomName string, gameName string, emulatorName string, logger logr.Logger) int {
	g.Logger = logger.WithValues("game", gameName, "room", roomName, "emulator", emulatorName)
	port := g.createTCPServer(basePort, maxGames)
	if port == 0 {
		return port
	}
	if err := g.createUDPServer(); err != nil {
		g.Logger.Error(err, "error creating UDP server")
		if err := g.TCPListener.Close(); err != nil && !g.isConnClosed(err) {
			g.Logger.Error(err, "error closing TcpListener")
		}
		return 0
	}
	return port
}

func (g *GameServer) CloseServers() {
	if err := g.UDPListener.Close(); err != nil && !g.isConnClosed(err) {
		g.Logger.Error(err, "error closing UdpListener")
	} else if err == nil {
		g.Logger.Info("UDP server closed")
	}
	if err := g.TCPListener.Close(); err != nil && !g.isConnClosed(err) {
		g.Logger.Error(err, "error closing TcpListener")
	} else if err == nil {
		g.Logger.Info("TCP server closed")
	}
	if g.QuitChannel != nil {
		*g.QuitChannel <- true
	}
}

func (g *GameServer) isConnClosed(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}

func (g *GameServer) bufferHealthAverage(playerNumber int) (float32, error) {
	var bufferHealth float32
	if g.GameData.BufferHealth[playerNumber].Len() > 0 {
		for _, k := range g.GameData.BufferHealth[playerNumber].Keys() {
			value, _ := g.GameData.BufferHealth[playerNumber].Peek(k)
			bufferHealth += float32(value)
		}
		return bufferHealth / float32(g.GameData.BufferHealth[playerNumber].Len()), nil
	} else {
		return 0, fmt.Errorf("no buffer health data for player %d", playerNumber)
	}
}

func (g *GameServer) ManageBuffer() {
	for {
		if !g.Running {
			g.Logger.Info("done managing buffers")
			return
		}

		// Find the largest buffer health
		var bufferHealth float32
		var foundPlayer bool
		g.GameDataMutex.Lock() // BufferHealth can be modified by processUDP in a different thread
		for i := range 4 {
			if g.GameData.CountLag[i] == 0 {
				playerBufferHealth, err := g.bufferHealthAverage(i)
				if err == nil && playerBufferHealth > bufferHealth {
					bufferHealth = playerBufferHealth
					foundPlayer = true
				}
			}
		}
		g.GameDataMutex.Unlock()

		if foundPlayer {
			if bufferHealth > float32(g.BufferTarget)+0.5 && g.GameData.BufferSize > 0 {
				g.GameData.BufferSize--
				g.Logger.Info("reduced buffer size", "bufferHealth", bufferHealth, "bufferSize", g.GameData.BufferSize)
			} else if bufferHealth < float32(g.BufferTarget)-0.5 {
				g.GameData.BufferSize++
				g.Logger.Info("increased buffer size", "bufferHealth", bufferHealth, "bufferSize", g.GameData.BufferSize)
			}
		}

		time.Sleep(time.Second)
	}
}

func (g *GameServer) ManagePlayers() {
	time.Sleep(time.Second * DisconnectTimeoutS)
	for {
		playersActive := false // used to check if anyone is still around
		var i byte

		g.GameDataMutex.Lock() // PlayerAlive and Status can be modified by processUDP in a different thread
		for i = range 4 {
			_, ok := g.Registrations[i]
			if ok {
				if g.GameData.PlayerAlive[i] {
					playerBufferHealth, _ := g.bufferHealthAverage(int(i))
					g.Logger.Info("player status", "player", i, "regID", g.Registrations[i].RegID, "bufferSize", g.GameData.BufferSize, "bufferHealth", playerBufferHealth, "countLag", g.GameData.CountLag[i], "address", g.GameData.PlayerAddresses[i])
					playersActive = true
				} else {
					g.Logger.Info("player disconnected UDP", "player", i, "regID", g.Registrations[i].RegID, "address", g.GameData.PlayerAddresses[i])
					g.GameData.Status |= (0x1 << (i + 1))

					g.RegistrationsMutex.Lock() // Registrations can be modified by processTCP
					delete(g.Registrations, i)
					g.RegistrationsMutex.Unlock()

					for k, v := range g.Players {
						if v.Number == int(i) {
							g.PlayersMutex.Lock()
							delete(g.Players, k)
							g.NeedsUpdatePlayers = true
							g.PlayersMutex.Unlock()
						}
					}
					g.GameData.BufferHealth[i].Purge()
				}
			}
			g.GameData.PlayerAlive[i] = false
		}
		g.GameDataMutex.Unlock()

		if !playersActive {
			g.Logger.Info("no more players, closing room", "numPlayers", g.NumberOfPlayers, "clientSHA", g.ClientSha, "playTime", time.Since(g.StartTime).String())
			g.CloseServers()
			g.Running = false
			return
		}
		time.Sleep(time.Second * DisconnectTimeoutS)
	}
}
