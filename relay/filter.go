package relay

import "github.com/satbooth/satbooth/nostr"

// Filter is one REQ filter. A nil list means the field was absent; a non-nil empty list matches
// nothing (NIP-01). Since and Until are inclusive. Limit is not used by Matches.
type Filter struct {
	Kinds         []int
	Authors, P, E []string
	Since, Until  *int64
	Limit         *int
}

func inStrings(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// hasTag reports whether e has a tag named name whose first value is in list.
func hasTag(e *nostr.Event, name string, list []string) bool {
	for _, t := range e.Tags {
		if len(t) >= 2 && t[0] == name && inStrings(list, t[1]) {
			return true
		}
	}
	return false
}

// Matches reports whether e passes every field of the filter.
func (f *Filter) Matches(e *nostr.Event) bool {
	if f.Kinds != nil {
		found := false
		for _, k := range f.Kinds {
			if k == e.Kind {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.Authors != nil && !inStrings(f.Authors, e.PubKey) {
		return false
	}
	if f.P != nil && !hasTag(e, "p", f.P) {
		return false
	}
	if f.E != nil && !hasTag(e, "e", f.E) {
		return false
	}
	if f.Since != nil && e.CreatedAt < *f.Since {
		return false
	}
	if f.Until != nil && e.CreatedAt > *f.Until {
		return false
	}
	return true
}
