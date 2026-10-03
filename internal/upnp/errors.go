package upnp

import (
	"errors"
	"fmt"

	"github.com/huin/goupnp/soap"
)

// Errors returned by Client. SOAP faults wrap one of the code sentinels in a
// *FaultError; anything else that fails a call wraps ErrUnreachable.
// Codes are from the IGD WANIPConnection spec as listed in miniupnpc's
// upnperrors.c.
var (
	ErrUnreachable          = errors.New("router unreachable")
	ErrActionFailed         = errors.New("action failed")                        // 501
	ErrNotAuthorized        = errors.New("action not authorized")                // 606
	ErrEndOfList            = errors.New("specified array index invalid")        // 713
	ErrNoSuchEntry          = errors.New("no such entry in array")               // 714
	ErrConflict             = errors.New("conflict in mapping entry")            // 718
	ErrSamePortRequired     = errors.New("same port values required")            // 724
	ErrOnlyPermanentLeases  = errors.New("only permanent leases supported")      // 725
	ErrExternalPortWildcard = errors.New("external port only supports wildcard") // 727
)

var faultCodes = map[int]error{
	501: ErrActionFailed,
	606: ErrNotAuthorized,
	713: ErrEndOfList,
	714: ErrNoSuchEntry,
	718: ErrConflict,
	724: ErrSamePortRequired,
	725: ErrOnlyPermanentLeases,
	727: ErrExternalPortWildcard,
}

// FaultError is a UPnP SOAP fault returned by the router.
type FaultError struct {
	Code        int
	Description string
	sentinel    error
}

func (e *FaultError) Error() string {
	return fmt.Sprintf("UPnP error %d: %s", e.Code, e.Description)
}

func (e *FaultError) Unwrap() error { return e.sentinel }

// Classify converts an error from goupnp into this package's errors. goupnp
// formats transport errors with %v, so anything that is not a SOAP fault is
// treated as the router being unreachable.
func Classify(err error) error {
	if err == nil {
		return nil
	}
	var fe *FaultError
	if errors.As(err, &fe) || errors.Is(err, ErrUnreachable) {
		return err
	}
	for _, s := range faultCodes {
		if errors.Is(err, s) {
			return err
		}
	}
	var sf *soap.SOAPFaultError
	if errors.As(err, &sf) {
		code := sf.Detail.UPnPError.Errorcode
		desc := sf.Detail.UPnPError.ErrorDescription
		if desc == "" {
			desc = sf.FaultString
		}
		return &FaultError{Code: code, Description: desc, sentinel: faultCodes[code]}
	}
	return fmt.Errorf("%w: %w", ErrUnreachable, err)
}
