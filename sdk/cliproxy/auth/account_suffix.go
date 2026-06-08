package auth

import "strings"

// AccountModelSeparator separates a base model name from a per-account email
// suffix in the public model name, e.g. "gpt-5.4 - rhys@example.com".
const AccountModelSeparator = " - "

// AccountEmail returns the OAuth account email associated with an auth, or "".
// It checks runtime Metadata first, then immutable Attributes (some storage
// backends persist the email there), so it works both at model-registration
// time and during execution.
func (a *Auth) AccountEmail() string {
	if a == nil {
		return ""
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["email"].(string); ok {
			if e := strings.TrimSpace(v); e != "" {
				return e
			}
		}
	}
	if a.Attributes != nil {
		if e := strings.TrimSpace(a.Attributes["email"]); e != "" {
			return e
		}
	}
	return ""
}

// SplitAccountModelSuffix splits a public model name of the form
// "<base> - <email>" into its base model and account email. ok is false when
// the name has no account suffix. The match requires the suffix to look like an
// email (contains '@', no whitespace) so legitimate names containing " - " are
// not misparsed; the last separator occurrence is used.
func SplitAccountModelSuffix(model string) (base string, email string, ok bool) {
	idx := strings.LastIndex(model, AccountModelSeparator)
	if idx < 0 {
		return model, "", false
	}
	base = strings.TrimSpace(model[:idx])
	email = strings.TrimSpace(model[idx+len(AccountModelSeparator):])
	if base == "" || !strings.Contains(email, "@") || strings.ContainsAny(email, " \t") {
		return model, "", false
	}
	return base, email, true
}
