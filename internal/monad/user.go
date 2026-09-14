package monad

import (
	"os/user"
	"strconv"
)

func monadIDs() (int, int, bool) {
	u, err := user.Lookup("monad")
	if err != nil {
		return 0, 0, false
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return uid, gid, true
}
