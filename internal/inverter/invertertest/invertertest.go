// Package invertertest holds DERControlList fixtures shared by the
// inverter and inverterclient tests.
package invertertest

import (
	"bytes"
	"encoding/xml"
	"errors"
	"net/http"
	"testing"

	"github.com/GRIDAPPSD/ieee-2030_5-core-go/pkg/sep2"
)

// BadFixedVar marks a control that WriteListWithBadItems turns into a
// refType 4 FixedVar, which core refuses on decode and its encoder refuses
// to write.
var BadFixedVar = sep2.FixedVar{RefType: 2, Value: 1234}

// WriteListWithBadItems writes list as sep+xml with every BadFixedVar
// rewritten to refType 4, failing the test unless exactly bad were found.
func WriteListWithBadItems(t testing.TB, w http.ResponseWriter, list sep2.DERControlList, bad int) {
	t.Helper()
	raw, err := xml.Marshal(&list)
	if err != nil {
		t.Errorf("encode DERControlList: %v", err)
		return
	}
	good := []byte("<refType>2</refType><value>1234</value>")
	if n := bytes.Count(raw, good); n != bad {
		t.Errorf("setup: %d marked FixedVars in the list, want %d", n, bad)
	}
	w.Header().Set("Content-Type", "application/sep+xml")
	if _, err := w.Write(bytes.ReplaceAll(raw, good, []byte("<refType>4</refType><value>1234</value>"))); err != nil {
		t.Errorf("write DERControlList: %v", err)
	}
}

// Rejected is a list item whose base failed to decode, as core reports it,
// with the error "bad base <mrid>".
func Rejected(mrid, replyTo string, mask sep2.HexBinary8) sep2.RejectedDERControl {
	var r sep2.RejectedDERControl
	r.MRID = mrid
	r.ReplyTo = replyTo
	r.ResponseRequired = &mask
	r.Err = errors.New("bad base " + mrid)
	return r
}

// ListResult is a decoded list holding controls and rejected items.
func ListResult(controls []sep2.DERControl, rejected ...sep2.RejectedDERControl) sep2.DERControlListResult {
	return sep2.DERControlListResult{DERControlList: sep2.DERControlList{DERControl: controls}, Rejected: rejected}
}
