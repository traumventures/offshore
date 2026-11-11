package provider

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorClass classifies a failed provider call (create, start). It
// decides what the orchestrator does with the provider:
//
//	capacity      no capacity for the size right now: cooldown with backoff
//	quota         a service quota or limit of the account is used up: cooldown with backoff
//	account_limit the account type does not allow it (e.g. OCI free tier
//	              "LimitExceeded ... upgrade"): disabled until the
//	              configuration changes or an operator resets it
//	auth          credentials missing, expired or not permitted: disabled
//	              like account_limit
//	transient     timeouts, throttling, 5xx: retried with the create backoff
//	other         anything else (configuration, image): create backoff
type ErrorClass string

const (
	ClassCapacity     ErrorClass = "capacity"
	ClassQuota        ErrorClass = "quota"
	ClassAccountLimit ErrorClass = "account_limit"
	ClassAuth         ErrorClass = "auth"
	ClassTransient    ErrorClass = "transient"
	ClassOther        ErrorClass = "other"
)

// Disables reports whether errors of the class disable the provider until
// a configuration change or a manual reset (they do not heal by waiting).
func (c ErrorClass) Disables() bool { return c == ClassAccountLimit || c == ClassAuth }

// CoolsDown reports whether errors of the class take the provider out of
// scale-out for a cooldown.
func (c ErrorClass) CoolsDown() bool { return c == ClassCapacity || c == ClassQuota }

// Sentinels that providers wrap (with %w) to state the class of an error
// explicitly; ErrCapacity is the capacity class.
var (
	ErrQuota        = errors.New("quota exceeded")
	ErrAccountLimit = errors.New("account limit")
	ErrAuth         = errors.New("not authorized")
	ErrTransient    = errors.New("transient provider error")
)

// ClassError wraps err with the sentinel of class c.
func ClassError(c ErrorClass, err error) error {
	var s error
	switch c {
	case ClassCapacity:
		s = ErrCapacity
	case ClassQuota:
		s = ErrQuota
	case ClassAccountLimit:
		s = ErrAccountLimit
	case ClassAuth:
		s = ErrAuth
	case ClassTransient:
		s = ErrTransient
	default:
		return err
	}
	return fmt.Errorf("%w: %w", s, err)
}

// Classify returns the class of a provider error: the wrapped sentinel if
// any, else from the error text (API error codes and messages of AWS EC2
// and OCI).
func Classify(err error) ErrorClass {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrAccountLimit):
		return ClassAccountLimit
	case errors.Is(err, ErrAuth):
		return ClassAuth
	case errors.Is(err, ErrQuota):
		return ClassQuota
	case errors.Is(err, ErrCapacity):
		return ClassCapacity
	case errors.Is(err, ErrTransient):
		return ClassTransient
	}
	return ClassifyText(err.Error())
}

// ClassifyText classifies an error message ("" or unknown: other).
func ClassifyText(msg string) ErrorClass {
	m := strings.ToLower(msg)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(m, s) {
				return true
			}
		}
		return false
	}
	switch {
	// The account cannot do it at all: free tier, trial, a limit only an
	// upgrade or a support request raises to more than zero.
	case has("upgrade your account", "upgrade to a paid", "please upgrade", "free tier", "free-tier", "trial account",
		"pendingverification", "account is not verified", "optinrequired", "blocked account", "account has been suspended"),
		has("limitexceeded") && has("upgrade"):
		return ClassAccountLimit
	case has("authfailure", "unauthorizedoperation", "notauthenticated", "notauthorized", "not authorized",
		"invalidclienttokenid", "expiredtoken", "signaturedoesnotmatch", "accessdenied", "access denied",
		"no valid credential", "credentials", "401 ", "403 ", "status code: 401", "status code: 403", "forbidden"):
		return ClassAuth
	case has("vcpulimitexceeded", "instancelimitexceeded", "quotaexceeded", "limitexceeded", "service limit",
		"maxspotinstancecountexceeded", "volumelimitexceeded", "addresslimitexceeded"):
		return ClassQuota
	case has("insufficientinstancecapacity", "insufficientcapacity", "insufficienthostcapacity",
		"insufficientreservedinstancecapacity", "out of host capacity", "out of capacity", "no capacity"):
		return ClassCapacity
	case has("requestlimitexceeded", "throttl", "toomanyrequests", "too many requests", "timeout", "timed out",
		"connection reset", "connection refused", "temporarily unavailable", "serviceunavailable", "internalerror",
		"internal error", "status code: 500", "status code: 502", "status code: 503", "status code: 504", "unexpected eof"):
		return ClassTransient
	}
	return ClassOther
}

// LaunchError is the error of a launch whose every machine type failed with
// a capacity-like error (failed: one entry per type): account_limit when
// any type hit an account limit, quota when all hit a quota, else
// capacity.
func LaunchError(backend, size string, failed []string) error {
	class, quota := ClassCapacity, len(failed) > 0
	for _, f := range failed {
		switch ClassifyText(f) {
		case ClassAccountLimit:
			class = ClassAccountLimit
		case ClassQuota:
		default:
			quota = false
		}
	}
	if class == ClassCapacity && quota {
		class = ClassQuota
	}
	return ClassError(class, fmt.Errorf("%s: no machine of size %s (%s)", backend, size, strings.Join(failed, "; ")))
}
