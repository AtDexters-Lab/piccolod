package container

import "os/user"

// CredentialResolver abstracts OS user/group lookup for testability.
// The default implementation delegates to the standard library's os/user package.
// Tests can replace defaultResolver to mock user resolution without root access.
type CredentialResolver interface {
	LookupUser(username string) (*user.User, error)
	LookupGroup(groupName string) (*user.Group, error)
}

type osCredentialResolver struct{}

func (osCredentialResolver) LookupUser(username string) (*user.User, error) {
	return user.Lookup(username)
}

func (osCredentialResolver) LookupGroup(groupName string) (*user.Group, error) {
	return user.LookupGroup(groupName)
}

// defaultResolver is the package-level resolver used by credential and appuser functions.
// Tests in this package can replace it with a mock but must not use t.Parallel()
// when doing so, since the variable is shared mutable state.
var defaultResolver CredentialResolver = osCredentialResolver{}
