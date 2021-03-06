package util

import "github.com/denisbrodbeck/machineid"

// MachineID returns protected id for the current machine
func MachineID() (string, error) {
	return machineid.ProtectedID("k0sproject-k0s")
}
