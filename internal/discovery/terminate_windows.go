package discovery

import "errors"

// Windows has no system responder child; nothing to stop.
func terminate(int) error { return errors.New("discovery: not supported on windows") }
