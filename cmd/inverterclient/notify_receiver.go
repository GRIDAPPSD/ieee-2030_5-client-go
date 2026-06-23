// IEEE-049 Phase 8 entry: HTTPS Notification receiver startup.
//
// Extracted from main.go so the receiver-start path can fail gracefully
// without growing the cmd/inverterclient log.Fatalf inventory (IEEE-048
// audit). The receiver is optional — when bind, cert load, or address
// resolution fails, we log and return nil; main() proceeds with polling-
// only (CSIP CORE-018 recommends subscription/notification but doesn't
// require it).
//
// This file is the boot-time wiring only. The receiver logic lives in
// internal/inverter/notify.go and is fully tested there.

package main

import (
	"log"

	"gitlab.pnnl.gov/arista/ieee-2030_5/ieee-2030_5-client/internal/inverter"
)

// startNotifyReceiver builds and starts the IEEE-049 inbound Notification
// listener. Returns nil on disabled (empty addr) or on any failure path,
// matching IEEE-048's graceful-bypass philosophy — the inverter can run
// in polling-only mode when the recommended-but-not-required subscription
// receiver can't come up.
//
// hmi may be nil; when non-nil, the chosen bind address is published to
// the dashboard via SetNotifyAddr.
//
// dispatcher is the per-Notification callback IEEE-049 hands every
// well-formed POST. main() passes a dispatch.RegisterableDispatcher (IEEESIM-004)
// constructed before Phase 5 wiring is done; the dispatcher self-handles
// the unregistered case (log + drop) so the listener can come up before
// the DERControlList href is known.
func startNotifyReceiver(
	cfg inverter.SimConfig,
	listenAddr string,
	hmi *inverter.HMI,
	dispatcher inverter.NotificationDispatcher,
) *inverter.NotifyReceiver {
	if listenAddr == "" {
		log.Println("Notification receiver: disabled (--notify-listen empty); subscription flow off, polling-only")
		return nil
	}

	if dispatcher == nil {
		// Fallback for callers that haven't been migrated yet. Safe default.
		dispatcher = inverter.NoopNotificationDispatcher
	}

	rcv, err := inverter.NewNotifyReceiver(inverter.NotifyReceiverConfig{
		CertFile:   cfg.CertFile,
		KeyFile:    cfg.KeyFile,
		CAFile:     cfg.CAFile,
		ListenAddr: listenAddr,
		Dispatcher: dispatcher,
	})
	if err != nil {
		log.Printf("Notification receiver: NewNotifyReceiver failed (%v); falling back to polling-only", err)
		return nil
	}
	if rcv == nil {
		// Defensive: NewNotifyReceiver returns nil only for empty
		// ListenAddr, which we already filtered. Treat as disabled.
		log.Println("Notification receiver: disabled by NewNotifyReceiver; falling back to polling-only")
		return nil
	}
	if err := rcv.Start(); err != nil {
		log.Printf("Notification receiver: Start failed (%v); falling back to polling-only", err)
		return nil
	}
	addr, err := rcv.Addr()
	if err != nil {
		log.Printf("Notification receiver: Addr failed (%v); falling back to polling-only", err)
		return nil
	}
	log.Printf("Notification receiver: https://%s/notify (IEEE-049)", addr)
	if hmi != nil {
		hmi.SetNotifyAddr(addr)
	}
	return rcv
}
