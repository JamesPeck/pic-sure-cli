package stack

// AuthFlags are the service and frontend switches derived from auth.mode
// (bash scripts/lib/config.sh, picsure_configure_auth). They are computed at
// render time and never stored.
type AuthFlags struct {
	// OpenIDPProvider lets PSAMA issue anonymous sessions
	// (OPEN_IDP_PROVIDER_IS_ENABLED).
	OpenIDPProvider bool
	// GatewayOpenAccess lets unauthenticated requests through the gateway
	// (GATEWAY_OPEN_ACCESS_ENABLED).
	GatewayOpenAccess bool
	// PublicAccess allows anonymous Explore queries on the authenticated
	// HPDS backend (ENABLE_PUBLIC_ACCESS).
	PublicAccess bool
	// ViteOpen, ViteOpenExplorer and ViteDiscover are the frontend's
	// VITE_OPEN, VITE_OPEN_EXPLORER and VITE_DISCOVER.
	ViteOpen         bool
	ViteOpenExplorer bool
	ViteDiscover     bool
}

// DeriveAuthFlags returns the flags for mode. required turns everything
// off; open allows anonymous use of Discover; explore allows anonymous use
// of the query builder. An unknown mode gets required's flags, so nothing is
// opened by mistake; Validate rejects unknown modes anyway.
func DeriveAuthFlags(mode AuthMode) AuthFlags {
	switch mode {
	case AuthOpen:
		return AuthFlags{OpenIDPProvider: true, GatewayOpenAccess: true, ViteOpen: true, ViteDiscover: true}
	case AuthExplore:
		return AuthFlags{OpenIDPProvider: true, GatewayOpenAccess: true, PublicAccess: true, ViteOpen: true, ViteOpenExplorer: true}
	default:
		return AuthFlags{}
	}
}

// Env returns the flags as NAME=true|false environment entries, in a fixed
// order, for the compose environment.
func (f AuthFlags) Env() []string {
	return []string{
		envBool("OPEN_IDP_PROVIDER_IS_ENABLED", f.OpenIDPProvider),
		envBool("GATEWAY_OPEN_ACCESS_ENABLED", f.GatewayOpenAccess),
		envBool("ENABLE_PUBLIC_ACCESS", f.PublicAccess),
		envBool("VITE_OPEN", f.ViteOpen),
		envBool("VITE_OPEN_EXPLORER", f.ViteOpenExplorer),
		envBool("VITE_DISCOVER", f.ViteDiscover),
	}
}

func envBool(name string, v bool) string {
	if v {
		return name + "=true"
	}
	return name + "=false"
}
