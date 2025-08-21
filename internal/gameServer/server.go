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
	regID  uint32
	plugin byte
	raw    byte
}

type GameServer struct {
	StartTime          time.Time
	Players            map[string]Client
	PlayersMutex       sync.Mutex
	tcpListener        *net.TCPListener
	udpListener        *net.UDPConn
	registrations      map[byte]*Registration
	registrationsMutex sync.Mutex
	tcpMutex           sync.Mutex
	tcpFiles           map[string][]byte
	customData         map[byte][]byte
	Logger             logr.Logger
	GameName           string
	Password           string
	ClientSha          string
	MD5                string
	Emulator           string
	tcpSettings        []byte
	gameData           GameData
	gameDataMutex      sync.Mutex
	Port               int
	hasSettings        bool
	Running            bool
	Features           map[string]string
	NeedsUpdatePlayers bool
	NumberOfPlayers    int
	BufferTarget       byte
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
		if err := g.tcpListener.Close(); err != nil && !g.isConnClosed(err) {
			g.Logger.Error(err, "error closing TcpListener")
		}
		return 0
	}
	return port
}

func (g *GameServer) CloseServers() {
	if err := g.udpListener.Close(); err != nil && !g.isConnClosed(err) {
		g.Logger.Error(err, "error closing UdpListener")
	} else if err == nil {
		g.Logger.Info("UDP server closed")
	}
	if err := g.tcpListener.Close(); err != nil && !g.isConnClosed(err) {
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

func (g *GameServer) averageBufferHealth(playerNumber int) (float32, error) {
	defer func() { g.gameData.bufferHealth[playerNumber] = g.gameData.bufferHealth[playerNumber][:0] }()

	if len(g.gameData.bufferHealth[playerNumber]) > 0 {
		var total float32
		for _, value := range g.gameData.bufferHealth[playerNumber] {
			total += float32(value)
		}
		return total / float32(len(g.gameData.bufferHealth[playerNumber])), nil
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

		var bufferHealth float32 = -1.0
		var leadPlayer int
		g.gameDataMutex.Lock() // BufferHealth can be modified by processUDP in a different thread
		for i := range 4 {
			var err error
			g.gameData.averageBufferHealth[i], err = g.averageBufferHealth(i)
			if err == nil && g.gameData.countLag[i] == 0 {
				// if g.gameData.averageBufferHealth[i] > bufferHealth {
				if leadPlayer == 0 {
					bufferHealth = g.gameData.averageBufferHealth[i]
					leadPlayer = i + 1
				}
			}
		}
		g.gameDataMutex.Unlock()

		if leadPlayer > 0 {
			if bufferHealth > float32(g.BufferTarget)+0.75 && g.gameData.bufferSize > 0 {
				g.gameData.bufferSize--
				g.Logger.Info("reduced buffer size", "bufferHealth", bufferHealth, "bufferSize", g.gameData.bufferSize, "leadPlayer", leadPlayer)
			} else if bufferHealth < float32(g.BufferTarget)-0.75 {
				g.gameData.bufferSize++
				g.Logger.Info("increased buffer size", "bufferHealth", bufferHealth, "bufferSize", g.gameData.bufferSize, "leadPlayer", leadPlayer)
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

		g.gameDataMutex.Lock() // PlayerAlive and Status can be modified by processUDP in a different thread
		for i = range 4 {
			_, ok := g.registrations[i]
			if ok {
				if g.gameData.playerAlive[i] {
					g.Logger.Info("player status", "player", i, "regID", g.registrations[i].regID, "bufferHealth", g.gameData.averageBufferHealth[i], "bufferSize", g.gameData.bufferSize, "countLag", g.gameData.countLag[i], "address", g.gameData.playerAddresses[i])
					playersActive = true
				} else {
					g.Logger.Info("player disconnected UDP", "player", i, "regID", g.registrations[i].regID, "address", g.gameData.playerAddresses[i])
					g.gameData.status |= (0x1 << (i + 1))

					g.registrationsMutex.Lock() // Registrations can be modified by processTCP
					delete(g.registrations, i)
					g.registrationsMutex.Unlock()

					for k, v := range g.Players {
						if v.Number == int(i) {
							g.PlayersMutex.Lock()
							delete(g.Players, k)
							g.NeedsUpdatePlayers = true
							g.PlayersMutex.Unlock()
						}
					}
					g.gameData.bufferHealth[i] = g.gameData.bufferHealth[i][:0]
				}
			}
			g.gameData.playerAlive[i] = false
		}
		g.gameDataMutex.Unlock()

		if !playersActive {
			g.Logger.Info("no more players, closing room", "numPlayers", g.NumberOfPlayers, "clientSHA", g.ClientSha, "playTime", time.Since(g.StartTime).String())
			g.CloseServers()
			g.Running = false
			return
		}
		time.Sleep(time.Second * DisconnectTimeoutS)
	}
}
