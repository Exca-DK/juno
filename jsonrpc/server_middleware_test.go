package jsonrpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/NethermindEth/juno/jsonrpc"
	"github.com/NethermindEth/juno/utils"
	"github.com/go-playground/validator/v10"
	"github.com/stretchr/testify/require"
)

type testRequestReporter struct {
	method   string
	duration time.Duration
	count    int
	errCode  int
}

func (m *testRequestReporter) ReportRequestDuration(method string, duration time.Duration) {
	m.duration = duration
}

func (m *testRequestReporter) ReportRequest(method string) {
	m.method = method
	m.count++
}

func (m *testRequestReporter) ReportRequestError(method string, errCode int) {
	m.errCode = errCode
}

func TestServerRequestMiddleware(t *testing.T) {
	method := jsonrpc.Method{
		Name:   "subtract",
		Params: []jsonrpc.Parameter{{Name: "minuend"}, {Name: "subtrahend"}},
		Handler: func(a, b int) (int, *jsonrpc.Error) {
			return a - b, nil
		},
	}

	t.Run("SingleMiddleware", func(t *testing.T) {
		var (
			req = []byte(`{"jsonrpc": "2.0", "method": "subtract", "params": {"minuend": 42, "subtrahend": 23}, "id": 4}`)
			res = []byte(`{"jsonrpc":"2.0","result":19,"id":4}`)
		)
		reporter := &testRequestReporter{}
		server := jsonrpc.NewServer(1, utils.NewNopZapLogger()).WithValidator(validator.New()).WithRequestMiddleware(jsonrpc.MetricsReporterMiddleware(reporter))
		require.NoError(t, server.RegisterMethod(method))
		result, err := server.Handle(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, res, result)
		require.Equal(t, "subtract", reporter.method)
		require.Equal(t, 1, reporter.count)
	})

	t.Run("ChainedMiddleware", func(t *testing.T) {
		var (
			req = []byte(`{"jsonrpc": "2.0", "method": "subtract", "params": {"minuend": 42, "subtrahend": 23}, "id": 4}`)
			res = []byte(`{"jsonrpc":"2.0","result":19,"id":4}`)
		)
		reporter := &testRequestReporter{}
		server := jsonrpc.NewServer(1, utils.NewNopZapLogger()).
			WithValidator(validator.New()).
			WithRequestMiddleware(jsonrpc.MetricsReporterMiddleware(reporter)).
			WithRequestMiddleware(jsonrpc.MetricsReporterMiddleware(reporter))
		require.NoError(t, server.RegisterMethod(method))
		result, err := server.Handle(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, res, result)
		require.Equal(t, "subtract", reporter.method)
		require.Equal(t, 2, reporter.count)
	})
}
