//go:build !darwin

package resources

import "errors"

func readDarwinProcessArgs(int) ([]byte, error) {
	return nil, errors.New("Darwin process arguments unavailable")
}
