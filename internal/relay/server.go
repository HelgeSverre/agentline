package relay

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/HelgeSverre/agentline/internal/store"
)

func Serve(ctx context.Context, listener net.Listener, handler http.Handler, data store.Store) error {
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	var err error
	select {
	case err = <-serveDone:
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err = server.Shutdown(shutdownCtx)
		cancel()
		if serveErr := <-serveDone; err == nil && !errors.Is(serveErr, http.ErrServerClosed) {
			err = serveErr
		}
	}
	closeErr := data.Close()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	if err != nil {
		return err
	}
	return closeErr
}
