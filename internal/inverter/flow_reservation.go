package inverter

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/GRIDAPPSD/ieee-2030_5-client-go/internal/inverter/guard"
	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// The flow reservation calls below are the aggregator's own: they act on its
// own EndDevice and never for a managed device. Each forces an empty guard
// target, so a context that carries a managed device's target (WithTarget)
// cannot turn a reservation into an action for that device.

// PostFlowReservationRequest POSTs req to the aggregator's own
// FlowReservationRequestList at listHref and returns the Location the
// server assigned.
func (c *SEP2Client) PostFlowReservationRequest(ctx context.Context, listHref string, req sep2.FlowReservationRequest) (string, error) {
	if listHref == "" {
		return "", fmt.Errorf("FlowReservationRequestList href required")
	}
	location, _, err := c.Post(WithTarget(ctx, ""), guard.KindFlowReservationPost, listHref, &req)
	if err != nil {
		return "", fmt.Errorf("POST FlowReservationRequest: %w", err)
	}
	return location, nil
}

// GetFlowReservationResponses GETs the aggregator's own
// FlowReservationResponseList at listHref, asking for up to 255 entries as
// the DERControlList read does.
func (c *SEP2Client) GetFlowReservationResponses(ctx context.Context, listHref string) (sep2.FlowReservationResponseList, error) {
	if listHref == "" {
		return sep2.FlowReservationResponseList{}, fmt.Errorf("FlowReservationResponseList href required")
	}
	sep := "?"
	if strings.Contains(listHref, "?") {
		sep = "&"
	}
	var list sep2.FlowReservationResponseList
	if _, err := c.Get(WithTarget(ctx, ""), guard.KindEndDeviceRead, listHref+sep+"l=255", &list); err != nil {
		return sep2.FlowReservationResponseList{}, fmt.Errorf("GET FlowReservationResponseList: %w", err)
	}
	return list, nil
}

// PostFlowReservationResponseResponse acknowledges a FlowReservationResponse
// by POSTing resp to the response's replyTo. resp.Subject is the mRID of the
// response being acknowledged.
func (c *SEP2Client) PostFlowReservationResponseResponse(ctx context.Context, replyToHref string, resp sep2.FlowReservationResponseResponse) error {
	if replyToHref == "" {
		return fmt.Errorf("replyTo href required")
	}
	if err := c.guard.Allow(http.MethodPost, guard.Action{Kind: guard.KindFlowReservationResponsePost, TargetLFDI: ""}); err != nil {
		return fmt.Errorf("POST FlowReservationResponseResponse %s: %w", replyToHref, err)
	}
	return c.sendResponse(WithTarget(ctx, ""), replyToHref, resp)
}
