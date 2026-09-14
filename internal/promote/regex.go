package promote

import "regexp"

var seqInputRe = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
