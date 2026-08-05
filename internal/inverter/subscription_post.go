// Package inverter : inverter Subscription POST helper.
//
// CSIP V1.2 CORE-018 step 2 (pp 59-61). With the /notify listener
// up, the inverter registers subscriptions with the SEP2 server by POSTing
// a Subscription resource to EndDevice.SubscriptionListLink. On 201 the
// server returns the new subscription href via the Location header; the
// inverter persists it for the cancellation path.
//
// Servers that don't implement subscription/notification respond 405 Method
// Not Allowed. The inverter MUST fall back to polling on 405 : that's the
// CSIP V1.2 graceful-degradation contract against non-CSIP servers. Callers
// branch on errors.Is(err, ErrMethodNotAllowed) (the typed sentinel surfaced
// by classifyResponse) and continue polling without aborting.
//
// This file ships the helper + main.go wiring + tests. A companion piece plugs the
// real Phase-5 NotificationDispatcher into the /notify listener's no-op slot;
// another handles status=1 cancellation Notifications. Neither is in scope here.

package inverter

import (
	"context"
	"errors"
	"fmt"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// PostSubscription registers a new Subscription with the SEP2 server.
//
// subscriptionListHref is the path published by the server in
// EndDevice.SubscriptionListLink (typically "/edev/{id}/sub"). subscribedHref
// is the resource the inverter wants change-notifications for (typically the
// FSAList or DERList). notifyURL is the full https URL of the inverter's
// /notify endpoint (built from NotifyReceiver.Addr()).
//
// On 201 Created the server returns the new subscription href in the
// Location header; PostSubscription returns that value. The caller should
// persist it so it can DELETE on cancellation.
//
// On 405 Method Not Allowed the server doesn't support subscriptions;
// PostSubscription returns the underlying ErrMethodNotAllowed (wrapped with
// "%w" plus URL context). Callers match with errors.Is(err, ErrMethodNotAllowed)
// and continue polling : no other server-side state is created on a 405.
//
// On any other status PostSubscription returns the wrapped error from the
// underlying Post helper. Callers should log a warning and continue :
// subscription/notification is recommended-but-not-required per CSIP V1.2
// CORE-018, so a failed registration must NOT crash the inverter.
//
// 301 follow is inherited from (*SEP2Client).Post : a redirect on the
// subscription-list path is followed once and the post-redirect Location
// (the new subscription href, not the new list href) is returned.
func (c *SEP2Client) PostSubscription(
	ctx context.Context,
	subscriptionListHref string,
	subscribedHref string,
	notifyURL string,
) (newSubHref string, err error) {
	if subscriptionListHref == "" {
		return "", errors.New("PostSubscription: subscriptionListHref required")
	}
	if subscribedHref == "" {
		return "", errors.New("PostSubscription: subscribedHref required")
	}
	if notifyURL == "" {
		return "", errors.New("PostSubscription: notifyURL required")
	}

	// Build the minimum-required Subscription body per IEEE 2030.5 §10.13 /
	// CSIP V1.2 CORE-018 step 2. Encoding=0 selects XML; we leave Limit and
	// Condition unset (the server picks defaults and the IEEE 2030.5 spec
	// permits both to be omitted on POST).
	sub := sep2.Subscription{
		SubscribedResource: subscribedHref,
		NotificationURI:    notifyURL,
		Encoding:           sep2.EncodingXML,
	}

	// (*SEP2Client).Post handles XML marshalling, mTLS, keep-alive, and
	// one-shot 301 follow. We get the Location header on 201
	// directly; on 405 / other non-2xx we get a typed error from
	// classifyResponse.
	location, _, postErr := c.Post(ctx, subscriptionListHref, &sub)
	if postErr != nil {
		// errors.Is preserves the typed sentinel through the Post wrapper's
		// fmt.Errorf chain so callers above us can pattern-match.
		return "", fmt.Errorf("POST %s subscribe %s: %w", subscriptionListHref, subscribedHref, postErr)
	}

	return location, nil
}
