// SPDX-License-Identifier: AGPL-3.0-only

//go:build e2e

package harness

// RebindToURL returns a copy of d pointed at baseURL, keeping the same
// pinned trust and client certificate. Device.Rebind (client.go) only
// accepts a host-binary *Agent; the in-container group's image-upgrade
// rehearsal (internal/e2e/incontainer/contract_state_test.go, Task 11)
// RECREATES the agent's container rather than restarting the process it
// runs in, so RunAgentContainer (image.go) assigns the new container a
// fresh published port and there is no *Agent value to hand Rebind. Kept in
// its own file rather than added to client.go: this task's instructions ask
// for harness additions to land in a new file, so a concurrently-written
// sibling task's edits never have to be merged against this one.
func RebindToURL(d *Device, baseURL string) *Device {
	return &Device{
		ID:             d.ID,
		CAFingerprint:  d.CAFingerprint,
		CertCommonName: d.CertCommonName,
		CertSerialHex:  d.CertSerialHex,
		baseURL:        baseURL,
		tlsConfig:      d.tlsConfig,
		client:         d.client,
	}
}
