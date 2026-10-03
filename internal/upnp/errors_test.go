package upnp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/huin/goupnp/soap"
)

func fault(code int) error {
	f := &soap.SOAPFaultError{FaultCode: "s:Client", FaultString: "UPnPError"}
	f.Detail.UPnPError.Errorcode = code
	return f
}

func TestClassify_SOAPFaultCodes(t *testing.T) { // codes per miniupnpc upnperrors.c
	cases := map[int]error{
		713: ErrEndOfList,
		714: ErrNoSuchEntry,
		718: ErrConflict,
		606: ErrNotAuthorized,
		501: ErrActionFailed,
		725: ErrOnlyPermanentLeases,
		724: ErrSamePortRequired,
		727: ErrExternalPortWildcard,
	}
	for code, want := range cases {
		got := Classify(fault(code))
		if !errors.Is(got, want) {
			t.Errorf("code %d: got %v, want errors.Is %v", code, got, want)
		}
		var fe *FaultError
		if !errors.As(got, &fe) || fe.Code != code {
			t.Errorf("code %d: want *FaultError with code, got %#v", code, got)
		}
		if errors.Is(got, ErrUnreachable) {
			t.Errorf("code %d: a SOAP fault is not ErrUnreachable", code)
		}
	}
}

func TestClassify_UnknownFaultCode(t *testing.T) {
	got := Classify(fault(799))
	var fe *FaultError
	if !errors.As(got, &fe) || fe.Code != 799 {
		t.Fatalf("got %#v", got)
	}
	for _, s := range []error{ErrEndOfList, ErrNoSuchEntry, ErrConflict, ErrUnreachable, ErrActionFailed} {
		if errors.Is(got, s) {
			t.Errorf("unknown code matched %v", s)
		}
	}
}

func TestClassify_WrappedFault(t *testing.T) {
	if got := Classify(fmt.Errorf("ctx: %w", fault(714))); !errors.Is(got, ErrNoSuchEntry) {
		t.Fatalf("got %v", got)
	}
}

func TestClassify_TransportErrorsAreUnreachable(t *testing.T) {
	for _, err := range []error{
		errors.New("goupnp: error performing SOAP HTTP request: dial tcp 192.168.1.1:5000: connect: connection refused"),
		errors.New("goupnp: SOAP request got HTTP 404 Not Found"),
		errors.New("goupnp: error decoding response body: EOF"),
		context.DeadlineExceeded,
	} {
		got := Classify(err)
		if !errors.Is(got, ErrUnreachable) {
			t.Errorf("Classify(%v) = %v, want ErrUnreachable", err, got)
		}
		if got.Error() == ErrUnreachable.Error() {
			t.Errorf("Classify(%v) dropped the cause: %v", err, got)
		}
	}
}

func TestClassify_AlreadyClassifiedUnchanged(t *testing.T) {
	for _, err := range []error{ErrNoSuchEntry, fmt.Errorf("x: %w", ErrUnreachable), Classify(fault(718))} {
		if got := Classify(err); got != err { //nolint:errorlint // identity: an already-classified error must come back unchanged
			t.Errorf("Classify(%v) = %v, want unchanged", err, got)
		}
	}
}

func TestClassify_Nil(t *testing.T) {
	if Classify(nil) != nil {
		t.Fatal("want nil")
	}
}
