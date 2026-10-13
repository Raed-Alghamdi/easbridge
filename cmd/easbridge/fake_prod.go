//go:build !devfake

package main

import (
	"errors"
	"log/slog"

	"easbridge/internal/config"
	"easbridge/internal/engine"
	"easbridge/internal/upstream"
)

// The production binary has no fake upstream: it is compiled only with the
// devfake build tag.
func newFakeFactory(*config.Config, *slog.Logger) (upstream.Factory, func(), error) {
	return nil, nil, errors.New("EASBRIDGE_UPSTREAM=fake needs a binary built with -tags devfake; this build only speaks to Exchange")
}

// tuneEngineForTests changes nothing in the production binary.
func tuneEngineForTests(*engine.Config, *slog.Logger) {}
