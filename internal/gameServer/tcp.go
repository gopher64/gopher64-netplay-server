package gameserver

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

type TCPData struct {
	filename       string
	buffer         bytes.Buffer
	filesize       uint32
	request        byte
	customID       byte
	customDatasize uint32
}

const (
	SettingsSize = 24
	TCPTimeout   = time.Minute * 5
)

const (
	RequestNone                = 255
	RequestSendSave            = 1
	RequestReceiveSave         = 2
	RequestSendSettings        = 3
	RequestReceiveSettings     = 4
	RequestRegisterPlayer      = 5
	RequestGetRegistration     = 6
	RequestDisconnectNotice    = 7
	RequestReceiveSaveWithSize = 8
	RequestSendCustomStart     = 64 // 64-127 are custom data send slots, 128-191 are custom data receive slots
	CustomDataOffset           = 64
)

func (g *GameServer) tcpSendFile(tcpData *TCPData, conn *net.TCPConn, withSize bool) {
	startTime := time.Now()
	var ok bool
	var data []byte
	for !ok {
		g.tcpMutex.Lock()
		data, ok = g.tcpFiles[tcpData.filename]
		g.tcpMutex.Unlock()
		if !ok {
			time.Sleep(time.Millisecond)
			if time.Since(startTime) > TCPTimeout {
				g.Logger.Info("TCP connection timed out in tcpSendFile")
				return
			}
		} else {
			if withSize {
				size := make([]byte, 4)
				binary.BigEndian.PutUint32(size, uint32(len(data)))
				_, err := conn.Write(size)
				if err != nil {
					g.Logger.Error(err, "could not write size", "address", conn.RemoteAddr().String())
				}
			}
			if len(data) > 0 {
				_, err := conn.Write(data)
				if err != nil {
					g.Logger.Error(err, "could not write file", "address", conn.RemoteAddr().String())
				}
			}

			// g.Logger.Info("sent save file", "filename", tcpData.filename, "filesize", tcpData.filesize, "address", conn.RemoteAddr().String())
			tcpData.filename = ""
			tcpData.filesize = 0
		}
	}
}

func (g *GameServer) tcpSendSettings(conn *net.TCPConn) {
	startTime := time.Now()
	for !g.hasSettings {
		time.Sleep(time.Millisecond)
		if time.Since(startTime) > TCPTimeout {
			g.Logger.Info("TCP connection timed out in tcpSendSettings")
			return
		}
	}
	_, err := conn.Write(g.tcpSettings)
	if err != nil {
		g.Logger.Error(err, "could not write settings", "address", conn.RemoteAddr().String())
	}
	// g.Logger.Info("sent settings", "address", conn.RemoteAddr().String())
}

func (g *GameServer) tcpSendCustom(conn *net.TCPConn, customID byte) {
	startTime := time.Now()
	var ok bool
	var data []byte
	for !ok {
		g.tcpMutex.Lock()
		data, ok = g.customData[customID]
		g.tcpMutex.Unlock()
		if !ok {
			time.Sleep(time.Millisecond)
			if time.Since(startTime) > TCPTimeout {
				g.Logger.Info("TCP connection timed out in tcpSendCustom")
				return
			}
		} else {
			_, err := conn.Write(data)
			if err != nil {
				g.Logger.Error(err, "could not write data", "address", conn.RemoteAddr().String())
			}
		}
	}
}

func (g *GameServer) tcpSendReg(conn *net.TCPConn) {
	startTime := time.Now()
	for len(g.Players) != len(g.registrations) {
		time.Sleep(time.Millisecond)
		if time.Since(startTime) > TCPTimeout {
			g.Logger.Info("TCP connection timed out in tcpSendReg")
			return
		}
	}
	var i byte
	registrations := make([]byte, 24)
	current := 0
	for i = range 4 {
		_, ok := g.registrations[i]
		if ok {
			binary.BigEndian.PutUint32(registrations[current:], g.registrations[i].regID)
			current += 4
			registrations[current] = g.registrations[i].plugin
			current++
			registrations[current] = g.registrations[i].raw
			current++
		} else {
			current += 6
		}
	}
	// g.Logger.Info("sent registration data", "address", conn.RemoteAddr().String())
	_, err := conn.Write(registrations)
	if err != nil {
		g.Logger.Error(err, "failed to send registration data", "address", conn.RemoteAddr().String())
	}
}

func (g *GameServer) processTCP(conn *net.TCPConn) {
	defer conn.Close() //nolint:errcheck

	tcpData := &TCPData{request: RequestNone}
	incomingBuffer := make([]byte, 1500)
	for {
		var readDeadline time.Time
		if tcpData.buffer.Len() > 0 {
			// if there is pending data in the buffer, get to it quickly
			readDeadline = time.Now().Add(time.Millisecond)
		} else {
			readDeadline = time.Now().Add(time.Second)
		}
		err := conn.SetReadDeadline(readDeadline)
		if err != nil {
			g.Logger.Error(err, "could not set read deadline", "address", conn.RemoteAddr().String())
		}
		length, err := conn.Read(incomingBuffer)
		if errors.Is(err, io.EOF) {
			// g.Logger.Info("Remote side closed TCP connection", "address", conn.RemoteAddr().String())
			return
		}
		if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
			g.Logger.Info("could not read TCP message", "reason", err.Error(), "address", conn.RemoteAddr().String())
			continue
		}
		if length > 0 {
			tcpData.buffer.Write(incomingBuffer[:length])
		}

		if tcpData.request == RequestNone { // find the request type
			if tcpData.buffer.Len() > 0 {
				tcpData.request, err = tcpData.buffer.ReadByte()
				if err != nil {
					g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
				}
			} else {
				continue // nothing to do
			}
		}

		if (tcpData.request == RequestSendSave || tcpData.request == RequestReceiveSave || tcpData.request == RequestReceiveSaveWithSize) && tcpData.filename == "" { // get file name
			if bytes.IndexByte(tcpData.buffer.Bytes(), 0) != -1 {
				filenameBytes, err := tcpData.buffer.ReadBytes(0)
				if err != nil {
					g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
				}
				tcpData.filename = string(filenameBytes[:len(filenameBytes)-1])
			}
		}

		if tcpData.request == RequestSendSave && tcpData.filename != "" && tcpData.filesize == 0 { // get file size from sender
			if tcpData.buffer.Len() >= 4 {
				filesizeBytes := make([]byte, 4)
				_, err = tcpData.buffer.Read(filesizeBytes)
				if err != nil {
					g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
				}
				tcpData.filesize = binary.BigEndian.Uint32(filesizeBytes)

				if tcpData.filesize == 0 {
					g.tcpMutex.Lock()
					g.tcpFiles[tcpData.filename] = make([]byte, tcpData.filesize)
					g.tcpMutex.Unlock()
					tcpData.filename = ""
					tcpData.filesize = 0
					tcpData.request = RequestNone
				}
			}
		}

		if tcpData.request == RequestSendSave && tcpData.filename != "" && tcpData.filesize != 0 { // read in file from sender
			if tcpData.buffer.Len() >= int(tcpData.filesize) {
				g.tcpMutex.Lock()
				g.tcpFiles[tcpData.filename] = make([]byte, tcpData.filesize)
				_, err = tcpData.buffer.Read(g.tcpFiles[tcpData.filename])
				g.tcpMutex.Unlock()
				if err != nil {
					g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
				}
				g.Logger.Info("received save file", "filename", tcpData.filename, "filesize", tcpData.filesize, "address", conn.RemoteAddr().String())
				tcpData.filename = ""
				tcpData.filesize = 0
				tcpData.request = RequestNone
			}
		}

		if tcpData.request == RequestReceiveSave && tcpData.filename != "" { // send requested file
			go g.tcpSendFile(tcpData, conn, false)
			tcpData.request = RequestNone
		}

		if tcpData.request == RequestReceiveSaveWithSize && tcpData.filename != "" { // send requested file
			go g.tcpSendFile(tcpData, conn, true)
			tcpData.request = RequestNone
		}

		if tcpData.request == RequestSendSettings { // get settings from P1
			if tcpData.buffer.Len() >= SettingsSize {
				_, err = tcpData.buffer.Read(g.tcpSettings)
				if err != nil {
					g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
				}
				// g.Logger.Info("read settings via TCP", "bufferLeft", tcpData.buffer.Len(), "address", conn.RemoteAddr().String())
				g.hasSettings = true
				tcpData.request = RequestNone
			}
		}

		if tcpData.request == RequestReceiveSettings { // send settings to P2-4
			go g.tcpSendSettings(conn)
			tcpData.request = RequestNone
		}

		if tcpData.request == RequestRegisterPlayer && tcpData.buffer.Len() >= 7 { // register player
			playerNumber, err := tcpData.buffer.ReadByte()
			if err != nil {
				g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
			}
			plugin, err := tcpData.buffer.ReadByte()
			if err != nil {
				g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
			}
			raw, err := tcpData.buffer.ReadByte()
			if err != nil {
				g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
			}
			regIDBytes := make([]byte, 4)
			_, err = tcpData.buffer.Read(regIDBytes)
			if err != nil {
				g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
			}
			regID := binary.BigEndian.Uint32(regIDBytes)

			response := make([]byte, 2)
			_, ok := g.registrations[playerNumber]
			if !ok {
				if playerNumber > 0 && plugin == 2 { // Only P1 can use mempak
					plugin = 1
				}

				g.registrationsMutex.Lock() // any player can modify this, which would be in a different thread
				g.registrations[playerNumber] = &Registration{
					regID:  regID,
					plugin: plugin,
					raw:    raw,
				}
				g.registrationsMutex.Unlock()

				response[0] = 1
				g.Logger.Info("registered player", "registration", g.registrations[playerNumber], "number", playerNumber, "bufferLeft", tcpData.buffer.Len(), "address", conn.RemoteAddr().String())

				g.gameDataMutex.Lock() // any player can modify this, which would be in a different thread
				g.gameData.pendingInput[playerNumber] = InputData{0, plugin}
				g.gameData.playerAlive[playerNumber] = true
				g.gameDataMutex.Unlock()
			} else {
				if g.registrations[playerNumber].regID == regID {
					g.Logger.Error(fmt.Errorf("re-registration"), "player already registered", "registration", g.registrations[playerNumber], "number", playerNumber, "bufferLeft", tcpData.buffer.Len(), "address", conn.RemoteAddr().String())
					response[0] = 1
				} else {
					g.Logger.Error(fmt.Errorf("registration failure"), "could not register player", "registration", g.registrations[playerNumber], "number", playerNumber, "bufferLeft", tcpData.buffer.Len(), "address", conn.RemoteAddr().String())
					response[0] = 0
				}
			}
			response[1] = uint8(g.BufferTarget)
			_, err = conn.Write(response)
			if err != nil {
				g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
			}
			tcpData.request = RequestNone
		}

		if tcpData.request == RequestGetRegistration { // send registration
			go g.tcpSendReg(conn)
			tcpData.request = RequestNone
		}

		if tcpData.request == RequestDisconnectNotice && tcpData.buffer.Len() >= 4 { // disconnect notice
			regIDBytes := make([]byte, 4)
			_, err = tcpData.buffer.Read(regIDBytes)
			if err != nil {
				g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
			}
			regID := binary.BigEndian.Uint32(regIDBytes)
			var i byte
			for i = range 4 {
				v, ok := g.registrations[i]
				if ok {
					if v.regID == regID {
						g.Logger.Info("player disconnected TCP", "regID", regID, "player", i, "address", conn.RemoteAddr().String())

						g.gameDataMutex.Lock() // any player can modify this, which would be in a different thread
						g.gameData.playerAlive[i] = false
						g.gameData.status |= (0x1 << (i + 1))

						g.registrationsMutex.Lock() // any player can modify this, which would be in a different thread
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
						g.gameDataMutex.Unlock()
					}
				}
			}
			tcpData.request = RequestNone
		}

		if tcpData.request >= RequestSendCustomStart && tcpData.request < RequestSendCustomStart+CustomDataOffset && tcpData.buffer.Len() >= 4 && tcpData.customID == 0 { // get custom data (for example, plugin settings)
			dataSizeBytes := make([]byte, 4)
			_, err = tcpData.buffer.Read(dataSizeBytes)
			if err != nil {
				g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
			}
			tcpData.customID = tcpData.request
			tcpData.customDatasize = binary.BigEndian.Uint32(dataSizeBytes)
		}

		if tcpData.request >= RequestSendCustomStart && tcpData.request < RequestSendCustomStart+CustomDataOffset && tcpData.customID != 0 { // read in custom data from sender
			if tcpData.buffer.Len() >= int(tcpData.customDatasize) {
				g.tcpMutex.Lock()
				g.customData[tcpData.customID] = make([]byte, tcpData.customDatasize)
				_, err = tcpData.buffer.Read(g.customData[tcpData.customID])
				g.tcpMutex.Unlock()
				if err != nil {
					g.Logger.Error(err, "TCP error", "address", conn.RemoteAddr().String())
				}
				tcpData.customID = 0
				tcpData.customDatasize = 0
				tcpData.request = RequestNone
			}
		}

		if tcpData.request >= RequestSendCustomStart+CustomDataOffset && tcpData.request < RequestSendCustomStart+CustomDataOffset+CustomDataOffset { // send custom data (for example, plugin settings)
			go g.tcpSendCustom(conn, tcpData.request-CustomDataOffset)
			tcpData.request = RequestNone
		}
	}
}

func (g *GameServer) watchTCP() {
	for {
		conn, err := g.tcpListener.AcceptTCP()
		if err != nil && !g.isConnClosed(err) {
			g.Logger.Error(err, "error from TcpListener")
			continue
		} else if g.isConnClosed(err) {
			return
		}

		if g.VerifyIP {
			validated := false
			remoteAddr, err := net.ResolveTCPAddr(conn.RemoteAddr().Network(), conn.RemoteAddr().String())
			if err != nil {
				g.Logger.Error(err, "could not resolve remote IP")
				conn.Close() //nolint:errcheck
				continue
			}
			for _, v := range g.Players {
				if remoteAddr.IP.Equal(v.IP) {
					validated = true
				}
			}
			if !validated {
				g.Logger.Error(fmt.Errorf("invalid tcp connection"), "bad IP", "IP", conn.RemoteAddr().String())
				conn.Close() //nolint:errcheck
				continue
			}
		}

		g.Logger.Info("received TCP connection", "address", conn.RemoteAddr().String())
		go g.processTCP(conn)
	}
}

func (g *GameServer) createTCPServer(basePort int, maxGames int) int {
	var err error
	for i := 1; i <= maxGames; i++ {
		g.tcpListener, err = net.ListenTCP("tcp", &net.TCPAddr{Port: basePort + i})
		if err == nil {
			g.Port = basePort + i
			g.Logger.Info("Created TCP server", "port", g.Port)
			g.tcpFiles = make(map[string][]byte)
			g.customData = make(map[byte][]byte)
			g.tcpSettings = make([]byte, SettingsSize)
			g.registrations = map[byte]*Registration{}
			go g.watchTCP()
			return g.Port
		}
	}
	return 0
}
