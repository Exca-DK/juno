package jsonrpc

import (
	"net/http"

	metrics "github.com/NethermindEth/juno/metrics/base"
	"github.com/NethermindEth/juno/utils"
)

const MaxRequestBodySize = 10 * 1024 * 1024 // 10MB

type HTTP struct {
	rpc      *Server
	log      utils.SimpleLogger
	reporter httpReporter
}

func NewHTTP(rpc *Server, log utils.SimpleLogger, factory metrics.Factory) *HTTP {
	h := &HTTP{
		rpc:      rpc,
		log:      log,
		reporter: newHttpReporter(factory),
	}

	return h
}

// ServeHTTP processes an incoming HTTP request
func (h *HTTP) ServeHTTP(writer http.ResponseWriter, req *http.Request) {
	if req.Method == "GET" {
		status := http.StatusNotFound
		if req.URL.Path == "/" {
			status = http.StatusOK
		}
		writer.WriteHeader(status)
		return
	} else if req.Method != "POST" {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	req.Body = http.MaxBytesReader(writer, req.Body, MaxRequestBodySize)
	h.reporter.requests.Inc()
	resp, err := h.rpc.HandleReader(req.Context(), req.Body)
	writer.Header().Set("Content-Type", "application/json")
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
	}
	if resp != nil {
		_, err = writer.Write(resp)
		if err != nil {
			h.log.Warnw("Failed writing response", "err", err)
		}
	}
}
