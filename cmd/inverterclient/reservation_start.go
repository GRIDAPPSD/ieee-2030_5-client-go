package main

import (
	"log"
	"time"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter"
	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/simconfig"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// startReservations builds the aggregator's reserver and gives the managed
// fleet the dispatcher that plans battery setpoints under a grant. It
// returns nil, after logging why, when the aggregator cannot reserve: no
// sim config (the reservation settings live there) or an EndDevice with no
// flow reservation links. A nil reserver makes SIGUSR1 a logged no-op.
func startReservations(client *inverter.SEP2Client, edev sep2.EndDevice, simFile *simconfig.File, fleet *managedFleet) *reserver {
	if simFile == nil {
		log.Println("aggregator role: no sim config; flow reservations are off and SIGUSR1 is ignored")
		return nil
	}
	disp := newDispatcher(time.Duration(simFile.Dispatch.TickS) * time.Second)
	r, err := newReserver(client, client.LFDI(), edev, simFile.Frq, disp, client.Now)
	if err != nil {
		log.Printf("aggregator role: flow reservations are off and SIGUSR1 is ignored: %v", err)
		return nil
	}
	fleet.setDispatcher(disp)
	if fleet == nil {
		log.Println("aggregator role: no managed devices; a granted reservation will be followed but nothing is dispatched")
	}
	log.Printf("aggregator role: flow reservations on: send SIGUSR1 to post one (request file %s)", simFile.Frq.RequestFile)
	return r
}
