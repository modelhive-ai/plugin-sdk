package plugin

import "context"

// IdentityProvider is the runtime part of an identity-provider plugin (Entra
// ID, Okta, any OIDC directory): people sign in to Modelhive with it, and its
// groups decide their teams and roles.
//
// The gateway runs the OpenID Connect flow itself, with the issuer and client
// the plugin names; the plugin never handles the browser redirect or the
// tokens. It only says where to authenticate and how to read who came back,
// which is the part that differs between directories (Entra's group
// overage, Okta's groups claim, custom claims).
type IdentityProvider interface {
	// OIDC returns the relying-party settings for the instance.
	OIDC(ctx context.Context) (OIDCSettings, error)
	// MapIdentity turns the verified ID token claims (and userinfo, merged)
	// into a Modelhive identity. Returning an error refuses the sign-in.
	MapIdentity(ctx context.Context, claims map[string]any) (Identity, error)
}

// OIDCSettings tell the gateway how to authenticate against the directory.
type OIDCSettings struct {
	// Issuer is the discovery base, e.g. https://login.microsoftonline.com/{tenant}/v2.0.
	Issuer       string
	ClientID     string
	ClientSecret string
	// Scopes beyond "openid", e.g. "profile", "email", "groups".
	Scopes []string
	// DisplayName labels the sign-in button.
	DisplayName string
}

// Identity is a federated person as Modelhive records them.
type Identity struct {
	// Subject is the directory's stable user ID; it links the Modelhive user
	// across email changes.
	Subject string
	Email   string
	// EmailVerified is true only when the token proves the directory verified
	// Email (Okta email_verified, Entra xms_edov). An unverified email still
	// names a new user but never links the sign-in to an existing one.
	EmailVerified bool
	// Name is the full display name; GivenName and FamilyName, when the
	// directory supplies them, take precedence over splitting Name.
	Name       string
	GivenName  string
	FamilyName string
	// Department is where the person works (the department claim), empty
	// when the directory does not send it.
	Department string
	// Groups are the directory group IDs or names the person belongs to.
	Groups []string
	// Team is the Modelhive team name the person belongs to, from a claim;
	// empty when none applies.
	Team string
	// Role is the Modelhive console role the group mapping gives: the highest
	// of the person's mapped groups, member when they are in none. Empty when
	// the plugin maps no roles, which leaves the role as it is.
	Role string
}

// DirectorySyncer is implemented by an IdentityProvider that can list the
// directory, so Modelhive learns of groups and leavers without waiting for a
// sign-in. Sync runs periodically with the instance's Env.HTTPClient.
type DirectorySyncer interface {
	Sync(ctx context.Context) (Directory, error)
}

// Directory is a snapshot of the people and groups a directory exposes.
type Directory struct {
	Users  []Identity
	Groups []DirectoryGroup
	// Complete is set when Users holds every person in scope, so the gateway
	// may disable Modelhive users the directory no longer lists.
	Complete bool
	// RolesComplete is set when Users holds every member of every role-mapped
	// group: a linked person missing from it is in none and becomes a member.
	RolesComplete bool
}

// DirectoryGroup is one group of the directory.
type DirectoryGroup struct {
	ID   string
	Name string
}
