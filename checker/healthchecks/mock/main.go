package mock

import (
	"context"

	"github.com/zinfra/srv-announcer/checker/healthchecks"
)

// Healthcheck exposes a HealthC channel, which is is piped through from Run()
type Healthcheck struct {
	HealthC chan bool
}

// ensure Healthcheck implements IHealthcheck
var _ healthchecks.IHealthcheck = &Healthcheck{}

func (h *Healthcheck) Run(ctx context.Context, healthyChan chan<- bool) {
	for {
		select {
		case <-ctx.Done():
			return
		case health := <-h.HealthC:
			healthyChan <- health
		}
	}
}
