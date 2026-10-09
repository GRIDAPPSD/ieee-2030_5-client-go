package main

import (
	"context"
	"log"
	"os"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
)

// operatorSignals registers the operator's reservation signals, SIGUSR1 to
// post and SIGUSR2 to withdraw. Only the aggregator registers them; in other
// roles both channels are nil and never fire.
func operatorSignals(cfg inverter.SimConfig) (reserve, withdraw <-chan os.Signal) {
	if !skipDERPipelineForRole(cfg) {
		return nil, nil
	}
	return reserveSignals(), withdrawSignals()
}

// withdrawOnSignal handles one SIGUSR2.
func withdrawOnSignal(ctx context.Context, r *reserver) {
	if r == nil {
		log.Println("flow reservation: SIGUSR2 ignored: flow reservations are off (see the start-up log)")
		return
	}
	r.Withdraw(ctx)
}
