package gameserver

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type GameData struct {
	SyncValues      map[uint32][]byte
	PlayerAddresses []*net.UDPAddr
	BufferSize      []uint32
	BufferHealth    []int32
	Inputs          []*lru.Cache[uint32, uint32]
	Plugin          []*lru.Cache[uint32, byte]
	PendingInput    []uint32
	CountLag        []uint32
	PendingPlugin   []byte
	PlayerAlive     []bool
	LeadCount       uint32
	Status          byte
}

const (
	KeyInfoClient           = 0
	KeyInfoServer           = 1
	PlayerInputRequest      = 2
	KeyInfoServerGratuitous = 3
	CP0Info                 = 4
)

const (
	StatusDesync           = 1
	DisconnectTimeoutS     = 30
	NoRegID                = 255
	InputDataMax       int = 5000
	CS4                    = 32
)

// returns true if v is bigger than w (accounting for uint32 wrap around).
func uintLarger(v uint32, w uint32) bool {
	return (w - v) > (math.MaxUint32 / 2) //nolint:gomnd,mnd
}

func (g *GameServer) getPlayerNumberByID(regID uint32) (byte, error) {
	var i byte
	for i = range 4 {
		v, ok := g.Registrations[i]
		if ok {
			if v.RegID == regID {
				return i, nil
			}
		}
	}
	return NoRegID, fmt.Errorf("could not find ID")
}

func (g *GameServer) fillInput(playerNumber byte, count uint32) uint32 {
	input, inputExists := g.GameData.Inputs[playerNumber].Get(count)
	if !inputExists {
		input = g.GameData.PendingInput[playerNumber]
		g.GameData.Inputs[playerNumber].Add(count, input)
		g.GameData.Plugin[playerNumber].Add(count, g.GameData.PendingPlugin[playerNumber])
	}
	return input
}

func (g *GameServer) sendUDPInput(count uint32, addr *net.UDPAddr, playerNumber byte, spectator bool, sendingPlayerNumber byte) uint32 {
	buffer := make([]byte, 508) //nolint:gomnd,mnd
	var countLag uint32
	if uintLarger(count, g.GameData.LeadCount) {
		if !spectator {
			g.Logger.Error(fmt.Errorf("bad count lag"), "count is larger than LeadCount", "count", count, "LeadCount", g.GameData.LeadCount, "playerNumber", playerNumber)
		}
	} else {
		countLag = g.GameData.LeadCount - count
	}
	if sendingPlayerNumber == NoRegID { // if the incoming packet was KeyInfoClient, the regID isn't included in the packet
		sendingPlayerNumber = playerNumber
		buffer[0] = KeyInfoServerGratuitous // client will ignore countLag value in this case
	} else {
		buffer[0] = KeyInfoServer
	}
	buffer[1] = playerNumber
	buffer[2] = g.GameData.Status
	buffer[3] = uint8(countLag) //nolint:gosec
	currentByte := 5
	start := count
	end := start + g.GameData.BufferSize[sendingPlayerNumber]
	_, ok := g.GameData.Inputs[playerNumber].Get(count) // check if input exists for this count
	for (currentByte < len(buffer)-9) && ((!spectator && countLag == 0 && uintLarger(end, count)) || ok) {
		binary.BigEndian.PutUint32(buffer[currentByte:], count)
		currentByte += 4
		binary.BigEndian.PutUint32(buffer[currentByte:], g.fillInput(playerNumber, count))
		currentByte += 4
		buffer[currentByte], _ = g.GameData.Plugin[playerNumber].Get(count)
		currentByte++
		count++
		_, ok = g.GameData.Inputs[playerNumber].Get(count) // check if input exists for this count
	}

	if count > start {
		buffer[4] = uint8(count - start) //nolint:gosec // number of counts in packet
		_, err := g.UDPListener.WriteToUDP(buffer[0:currentByte], addr)
		if err != nil {
			g.Logger.Error(err, "could not send input")
		}
	}
	return countLag
}

func (g *GameServer) processUDP(addr *net.UDPAddr, buf []byte) {
	playerNumber := buf[1]
	if buf[0] == KeyInfoClient {
		g.GameData.PlayerAddresses[playerNumber] = addr
		count := binary.BigEndian.Uint32(buf[2:])

		g.GameData.PendingInput[playerNumber] = binary.BigEndian.Uint32(buf[6:])
		g.GameData.PendingPlugin[playerNumber] = buf[10]

		for i := range 4 {
			if g.GameData.PlayerAddresses[i] != nil {
				g.sendUDPInput(count, g.GameData.PlayerAddresses[i], playerNumber, true, NoRegID)
			}
		}
	} else if buf[0] == PlayerInputRequest {
		regID := binary.BigEndian.Uint32(buf[2:])
		count := binary.BigEndian.Uint32(buf[6:])
		spectator := buf[10]
		if uintLarger(count, g.GameData.LeadCount) && spectator == 0 {
			g.GameData.LeadCount = count
		}
		sendingPlayerNumber, err := g.getPlayerNumberByID(regID)
		if err != nil {
			g.Logger.Error(err, "could not process request", "regID", regID)
			return
		}
		countLag := g.sendUDPInput(count, addr, playerNumber, spectator != 0, sendingPlayerNumber)
		g.GameData.BufferHealth[sendingPlayerNumber] = int32(buf[11])

		g.GameDataMutex.Lock() // PlayerAlive can be modified by ManagePlayers in a different thread
		g.GameData.PlayerAlive[sendingPlayerNumber] = true
		g.GameDataMutex.Unlock()

		g.GameData.CountLag[sendingPlayerNumber] = countLag
	} else if buf[0] == CP0Info {
		if g.GameData.Status&StatusDesync == 0 {
			viCount := binary.BigEndian.Uint32(buf[1:])
			syncValue, ok := g.GameData.SyncValues[viCount]
			if !ok {
				g.GameData.SyncValues[viCount] = buf[5:133]
			} else if !bytes.Equal(syncValue, buf[5:133]) {
				g.GameDataMutex.Lock() // Status can be modified by ManagePlayers in a different thread
				g.GameData.Status |= StatusDesync
				g.GameDataMutex.Unlock()

				g.Logger.Error(fmt.Errorf("desync"), "game has desynced", "numPlayers", g.NumberOfPlayers, "clientSHA", g.ClientSha, "playTime", time.Since(g.StartTime).String(), "features", g.Features)
			}
		}
	}
}

func (g *GameServer) watchUDP() {
	for {
		buf := make([]byte, 1500) //nolint:gomnd,mnd
		_, addr, err := g.UDPListener.ReadFromUDP(buf)
		if err != nil && !g.isConnClosed(err) {
			g.Logger.Error(err, "error from UdpListener")
			continue
		} else if g.isConnClosed(err) {
			return
		}

		validated := false
		for _, v := range g.Players {
			if addr.IP.Equal(net.ParseIP(v.IP)) {
				validated = true
			}
		}
		if !validated {
			g.Logger.Error(fmt.Errorf("invalid udp connection"), "bad IP", "IP", addr.IP)
			continue
		}

		g.processUDP(addr, buf)
	}
}

func (g *GameServer) createUDPServer() error {
	var err error
	g.UDPListener, err = net.ListenUDP("udp", &net.UDPAddr{Port: g.Port})
	if err != nil {
		return err //nolint:wrapcheck
	}
	if err := ipv4.NewConn(g.UDPListener).SetTOS(CS4 << 2); err != nil { //nolint:gomnd,mnd
		g.Logger.Error(err, "could not set IPv4 DSCP")
	}
	if err := ipv6.NewConn(g.UDPListener).SetTrafficClass(CS4 << 2); err != nil { //nolint:gomnd,mnd
		g.Logger.Error(err, "could not set IPv6 DSCP")
	}
	g.Logger.Info("Created UDP server", "port", g.Port)

	g.GameData.PlayerAddresses = make([]*net.UDPAddr, 4) //nolint:gomnd,mnd
	g.GameData.BufferSize = []uint32{3, 3, 3, 3}
	g.GameData.BufferHealth = []int32{-1, -1, -1, -1}
	g.GameData.Inputs = make([]*lru.Cache[uint32, uint32], 4) //nolint:gomnd,mnd
	for i := range 4 {
		g.GameData.Inputs[i], _ = lru.New[uint32, uint32](InputDataMax)
	}
	g.GameData.Plugin = make([]*lru.Cache[uint32, byte], 4) //nolint:gomnd,mnd
	for i := range 4 {
		g.GameData.Plugin[i], _ = lru.New[uint32, byte](InputDataMax)
	}
	g.GameData.PendingInput = make([]uint32, 4) //nolint:gomnd,mnd
	g.GameData.PendingPlugin = make([]byte, 4)  //nolint:gomnd,mnd
	g.GameData.SyncValues = make(map[uint32][]byte)
	g.GameData.PlayerAlive = make([]bool, 4) //nolint:gomnd,mnd
	g.GameData.CountLag = make([]uint32, 4)  //nolint:gomnd,mnd

	go g.watchUDP()
	return nil
}
