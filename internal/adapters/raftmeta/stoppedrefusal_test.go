package raftmeta

import (
	"errors"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// refusedAsStopped reports whether err is how a request is refused by a
// state machine that stopped for cause: unavailable - another node can
// answer it - saying why, without the cause's domain errors, which would
// make a proposer take the refusal for another (fsm.health).
func refusedAsStopped(err, cause error) bool {
	return errors.Is(err, domain.ErrUnavailable) && !errors.Is(err, cause) && strings.Contains(err.Error(), cause.Error())
}
