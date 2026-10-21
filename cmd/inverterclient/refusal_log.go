package main

import "log"

// refusalLog logs a refused control when the reason changes, so a refusal
// that holds for every tick of an event is logged once, and its end is
// logged too.
type refusalLog struct{ last string }

func (l *refusalLog) observe(who string, refused error) {
	reason := ""
	if refused != nil {
		reason = refused.Error()
	}
	if reason == l.last {
		return
	}
	l.last = reason
	if refused != nil {
		log.Printf("%s: control refused: %v", who, refused)
		return
	}
	log.Printf("%s: control no longer refused", who)
}
