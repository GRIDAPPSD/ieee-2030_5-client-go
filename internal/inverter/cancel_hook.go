package inverter

// CancelHook is the seam invoked on status=1 notifications.
// Receives the cancelled subscription's subscribedResource href (the
// thing the inverter subscribed TO) so the caller can free its
// subscription-tracking state. Implementations MUST be safe for
// concurrent invocation: notifications arrive on independent net/http
// goroutines.
type CancelHook func(subscribedHref string)
