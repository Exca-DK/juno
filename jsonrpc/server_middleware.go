package jsonrpc

import (
	"context"
	"time"
)

type requestHandler func(ctx context.Context, req *request) (*response, error)

// requestMiddleware is a middleware between request and next requestHandler.
// the middleware needs to call the next
type requestMiddleware func(ctx context.Context, req *request, next requestHandler) (*response, error)

// WithRequestMiddleware registers a request middleware to intercept requests.
func (s *Server) WithRequestMiddleware(middleware requestMiddleware) *Server {
	handler := s.handler
	if handler == nil {
		handler = s.handleRequest
	}
	s.handler = func(ctx context.Context, req *request) (*response, error) { return middleware(ctx, req, handler) }
	return s
}

type requestReporter interface {
	ReportRequest(method string)
	ReportRequestError(method string, errCode int)
	ReportRequestDuration(method string, duration time.Duration)
}

// MetricsReporterMiddleware intercepts request and reports statistics to reporter.
func MetricsReporterMiddleware(reporter requestReporter) requestMiddleware {
	return func(ctx context.Context, req *request, next requestHandler) (*response, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		duration := time.Since(start)
		reporter.ReportRequest(req.Method)
		reporter.ReportRequestDuration(req.Method, duration)
		if resp != nil && resp.Error != nil {
			reporter.ReportRequestError(req.Method, resp.Error.Code)
		}
		return resp, err
	}
}
