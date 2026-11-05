package apps

import "github.com/orglane/orglane/providers/provider"

// EnvForTest returns the Env adapters get for an instance, so tests can read
// credential plaintext the way an adapter does.
func (s *Service) EnvForTest(inst Instance) provider.Env { return s.env(inst) }

// CredentialADForTest exposes the additional data a credential row is
// sealed with.
func CredentialADForTest(instanceID [16]byte, name string, version int32) []byte {
	return credentialAD(instanceID, name, version)
}
