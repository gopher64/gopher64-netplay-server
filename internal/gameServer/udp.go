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

type InputData struct {
	keys   uint32
	plugin byte
}

type GameData struct {
	syncValues          *lru.Cache[uint32, []byte]
	playerAddresses     [4]*net.UDPAddr
	bufferSize          uint32
	averageBufferHealth [4]float32
	averageCountLag     [4]float32
	bufferHealth        [4][]byte
	inputs              [4]*lru.Cache[uint32, InputData]
	pendingInput        [4]InputData
	countLag            [4][]uint32
	sendBuffer          []byte
	recvBuffer          []byte
	playerAlive         [4]bool
	leadCount           uint32
	status              byte
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
	InputDataMax       int = 60 * 60 // One minute of input data
	CS4                    = 32
)

// returns true if v is bigger than w (accounting for uint32 wrap around).
func uintLarger(v uint32, w uint32) bool {
	return (w - v) > (math.MaxUint32 / 2)
}

func (g *GameServer) getPlayerNumberByID(regID uint32) (byte, error) {
	var i byte
	for i = range 4 {
		v, ok := g.registrations[i]
		if ok {
			if v.regID == regID {
				return i, nil
			}
		}
	}
	return NoRegID, fmt.Errorf("could not find ID")
}

func (g *GameServer) fillInput(playerNumber byte, count uint32) InputData {
	input, inputExists := g.gameData.inputs[playerNumber].Get(count)
	if !inputExists {
		input = g.gameData.pendingInput[playerNumber]
		g.gameData.inputs[playerNumber].Add(count, input)
	}
	return input
}

func (g *GameServer) sendUDPInput(count uint32, addr *net.UDPAddr, playerNumber byte, spectator bool, sendingPlayerNumber byte) uint32 {
	var countLag uint32
	if uintLarger(count, g.gameData.leadCount) {
		if !spectator {
			g.Logger.Error(fmt.Errorf("bad count lag"), "count is larger than LeadCount", "count", count, "LeadCount", g.gameData.leadCount, "playerNumber", playerNumber)
		}
	} else {
		countLag = g.gameData.leadCount - count
	}
	if sendingPlayerNumber == NoRegID { // if the incoming packet was KeyInfoClient, the regID isn't included in the packet
		g.gameData.sendBuffer[0] = KeyInfoServerGratuitous // client will ignore countLag value in this case
	} else {
		g.gameData.sendBuffer[0] = KeyInfoServer
	}
	g.gameData.sendBuffer[1] = playerNumber
	g.gameData.sendBuffer[2] = g.gameData.status
	g.gameData.sendBuffer[3] = uint8(countLag)
	currentByte := 5
	start := count
	end := start + g.gameData.bufferSize
	_, ok := g.gameData.inputs[playerNumber].Get(count) // check if input exists for this count
	for (currentByte < len(g.gameData.sendBuffer)-9) && ((!spectator && countLag == 0 && uintLarger(end, count)) || ok) {
		binary.BigEndian.PutUint32(g.gameData.sendBuffer[currentByte:], count)
		currentByte += 4
		input := g.fillInput(playerNumber, count)
		binary.BigEndian.PutUint32(g.gameData.sendBuffer[currentByte:], input.keys)
		currentByte += 4
		g.gameData.sendBuffer[currentByte] = input.plugin
		currentByte++
		count++
		_, ok = g.gameData.inputs[playerNumber].Get(count) // check if input exists for this count
	}

	if count > start {
		g.gameData.sendBuffer[4] = uint8(count - start) // number of counts in packet
		_, err := g.udpListener.WriteToUDP(g.gameData.sendBuffer[0:currentByte], addr)
		if err != nil {
			g.Logger.Error(err, "could not send input")
		}
	}
	return countLag
}

func (g *GameServer) processUDP(addr *net.UDPAddr) {
	playerNumber := g.gameData.recvBuffer[1]
	switch g.gameData.recvBuffer[0] {
	case KeyInfoClient:
		g.gameData.playerAddresses[playerNumber] = addr
		count := binary.BigEndian.Uint32(g.gameData.recvBuffer[2:])

		g.gameData.pendingInput[playerNumber] = InputData{
			keys:   binary.BigEndian.Uint32(g.gameData.recvBuffer[6:]),
			plugin: g.gameData.recvBuffer[10],
		}

		for i := range 4 {
			if g.gameData.playerAddresses[i] != nil {
				g.sendUDPInput(count, g.gameData.playerAddresses[i], playerNumber, true, NoRegID)
			}
		}
	case PlayerInputRequest:
		regID := binary.BigEndian.Uint32(g.gameData.recvBuffer[2:])
		count := binary.BigEndian.Uint32(g.gameData.recvBuffer[6:])
		spectator := g.gameData.recvBuffer[10]
		if uintLarger(count, g.gameData.leadCount) && spectator == 0 {
			g.gameData.leadCount = count
		}
		sendingPlayerNumber, err := g.getPlayerNumberByID(regID)
		if err != nil {
			g.Logger.Error(err, "could not process request", "regID", regID)
			return
		}
		g.gameDataMutex.Lock() // playerAlive, countLag and bufferHealth can be modified in different threads
		g.gameData.countLag[sendingPlayerNumber] = append(g.gameData.countLag[sendingPlayerNumber], g.sendUDPInput(count, addr, playerNumber, spectator != 0, sendingPlayerNumber))
		g.gameData.bufferHealth[sendingPlayerNumber] = append(g.gameData.bufferHealth[sendingPlayerNumber], g.gameData.recvBuffer[11])
		g.gameData.playerAlive[sendingPlayerNumber] = true
		g.gameDataMutex.Unlock()
	case CP0Info:
		if g.gameData.status&StatusDesync == 0 {
			viCount := binary.BigEndian.Uint32(g.gameData.recvBuffer[1:])
			syncValue, ok := g.gameData.syncValues.Get(viCount)
			if !ok {
				g.gameData.syncValues.Add(viCount, bytes.Clone(g.gameData.recvBuffer[5:133]))
			} else if !bytes.Equal(syncValue, g.gameData.recvBuffer[5:133]) {
				g.gameDataMutex.Lock() // Status can be modified by ManagePlayers in a different thread
				g.gameData.status |= StatusDesync
				g.gameDataMutex.Unlock()

				g.Logger.Error(fmt.Errorf("desync"), "game has desynced", "numPlayers", g.NumberOfPlayers, "clientSHA", g.ClientSha, "playTime", time.Since(g.StartTime).String(), "features", g.Features)
			}
		}
	}
}

func (g *GameServer) watchUDP() {
	for {
		_, addr, err := g.udpListener.ReadFromUDP(g.gameData.recvBuffer)
		if err != nil && !g.isConnClosed(err) {
			g.Logger.Error(err, "error from UdpListener")
			continue
		} else if g.isConnClosed(err) {
			return
		}

		validated := false
		for _, v := range g.Players {
			if addr.IP.Equal(v.IP) {
				validated = true
			}
		}
		if !validated {
			g.Logger.Error(fmt.Errorf("invalid udp connection"), "bad IP", "IP", addr.IP)
			continue
		}

		g.processUDP(addr)
	}
}

func (g *GameServer) createUDPServer() error {
	var err error
	g.udpListener, err = net.ListenUDP("udp", &net.UDPAddr{Port: g.Port})
	if err != nil {
		return err
	}
	if err := ipv4.NewConn(g.udpListener).SetTOS(CS4 << 2); err != nil {
		g.Logger.Error(err, "could not set IPv4 DSCP")
	}
	if err := ipv6.NewConn(g.udpListener).SetTrafficClass(CS4 << 2); err != nil {
		g.Logger.Error(err, "could not set IPv6 DSCP")
	}
	g.Logger.Info("Created UDP server", "port", g.Port)

	g.gameData.bufferSize = 3
	for i := range 4 {
		g.gameData.inputs[i], _ = lru.New[uint32, InputData](InputDataMax)
		g.gameData.bufferHealth[i] = make([]byte, 0, InputDataMax)
		g.gameData.countLag[i] = make([]uint32, 0, InputDataMax)
	}
	g.gameData.syncValues, _ = lru.New[uint32, []byte](100) // Store up to 100 sync values
	g.gameData.sendBuffer = make([]byte, 508)
	g.gameData.recvBuffer = make([]byte, 1500)

	go g.watchUDP()
	return nil
}
