package server

import (
	"net/http"

	"github.com/jooservices/speedtest/internal/center/mcpserver"
)

func newMCPHandler(api http.Handler) http.Handler { return mcpserver.NewHandler(api) }
