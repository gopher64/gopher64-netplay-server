package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/go-logr/zapr"
	lobbyserver "github.com/gopher64/gopher64-netplay-server/internal/lobbyServer"
	"go.uber.org/zap"
)

const (
	DefaultBasePort    = 45000
	DefaultMOTDMessage = "Please consider <a href=\"https://www.patreon.com/loganmc10\">subscribing to the Patreon</a> or " +
		"<a href=\"https://github.com/sponsors/loganmc10\">supporting this project on GitHub.</a> Your support is needed in order to keep the netplay service online."
)

func newZap(logPath string) (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	cfg.OutputPaths = []string{"stderr"}
	if logPath != "" {
		cfg.OutputPaths = append(cfg.OutputPaths, logPath)
	}
	return cfg.Build() //nolint:wrapcheck
}

func main() {
	name := flag.String("name", "local-server", "Server name")
	basePort := flag.Int("baseport", DefaultBasePort, "Base port")
	disableBroadcast := flag.Bool("disable-broadcast", false, "Disable LAN broadcast")
	logPath := flag.String("log-path", "", "Write logs to this file")
	motd := flag.String("motd", "", "MOTD message to display to clients")
	maxGames := flag.Int("max-games", 10, "Maximum number of concurrent games") //nolint:gomnd,mnd
	enableAuth := flag.Bool("enable-auth", false, "Enable client authentication")
	flag.Parse()

	zapLog, err := newZap(*logPath)
	if err != nil {
		log.Panic(err)
	}
	logger := zapr.NewLogger(zapLog)

	if *name == "" {
		logger.Error(fmt.Errorf("name required"), "server name not set")
		os.Exit(1)
	}

	if *motd == "" {
		*motd = DefaultMOTDMessage
	}

	s := lobbyserver.LobbyServer{
		Logger:           logger,
		Name:             *name,
		BasePort:         *basePort,
		DisableBroadcast: *disableBroadcast,
		Motd:             *motd,
		MaxGames:         *maxGames,
		EnableAuth:       *enableAuth,
	}
	go s.LogServerStats()
	if err := s.RunSocketServer(DefaultBasePort); err != nil {
		logger.Error(err, "could not run socket server")
	}
}
