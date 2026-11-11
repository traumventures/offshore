package adapter

import "github.com/traumventures/offshore/internal/access"

// AccessHinter is implemented by adapters that know how their tool uses
// the files of a build slot. Like fadvise/madvise hints, the priors seed
// the access classifier (internal/access) until it has learned the
// patterns itself.
type AccessHinter interface {
	// AccessPriors are class hints for slot-relative path globs, most
	// specific first.
	AccessPriors() []access.Prior
}

// AccessPriors returns the priors of all adapters in detection order
// (globs are ecosystem specific, so one list serves every slot).
func AccessPriors() []access.Prior {
	var out []access.Prior
	for _, a := range All {
		if h, ok := a.(AccessHinter); ok {
			out = append(out, h.AccessPriors()...)
		}
	}
	return out
}

// AccessPriors: cargo target directories (also below target/<triple>/).
// deps/ holds compiled dependencies (read-mostly), incremental/ and
// .fingerprint/ are rewritten every build (churn), build/ holds build
// script outputs (write-once), target/tmp is scratch space (ephemeral).
func (Cargo) AccessPriors() []access.Prior {
	return []access.Prior{
		{Glob: "**/target/**/incremental/**", Class: access.Churn},
		{Glob: "**/target/**/.fingerprint/**", Class: access.Churn},
		{Glob: "**/target/**/deps/**", Class: access.Immutable},
		{Glob: "**/target/**/build/**", Class: access.WriteOnce},
		{Glob: "**/target/tmp/**", Class: access.Ephemeral},
	}
}

// AccessPriors: the module cache is immutable, the build cache content
// addressed (write-once) except its trim stamp.
func (Go) AccessPriors() []access.Prior {
	return []access.Prior{
		{Glob: "**/go-build/trim.txt", Class: access.Churn},
		{Glob: "**/go-build/**", Class: access.WriteOnce},
		{Glob: "**/pkg/mod/**", Class: access.Immutable},
	}
}

// AccessPriors: installed packages are read-mostly, bundler and framework
// caches churn, turbo's content addressed cache is write-once.
func (Node) AccessPriors() []access.Prior {
	return []access.Prior{
		{Glob: "**/node_modules/.cache/**", Class: access.Churn},
		{Glob: "**/node_modules/.vite/**", Class: access.Churn},
		{Glob: "**/node_modules/**", Class: access.Immutable},
		{Glob: "**/.next/cache/**", Class: access.Churn},
		{Glob: "**/.parcel-cache/**", Class: access.Churn},
		{Glob: "**/.turbo/**", Class: access.WriteOnce},
	}
}
