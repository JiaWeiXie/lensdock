package supervisor

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"

	"github.com/JiaWeiXie/lensdock/internal/auth"
)

const (
	xauthFamilyWild  = 0xffff
	xauthProtocol    = "MIT-MAGIC-COOKIE-1"
	xauthCookieBytes = 16
)

// encodeXauthority returns one .Xauthority entry in the on-disk format:
// big-endian uint16 family, then length-prefixed (uint16) address, display
// number, protocol name and cookie. FamilyWild with an empty address matches
// any host, so the file works for every client in the container.
func encodeXauthority(displayNum int, cookie []byte) []byte {
	number := strconv.Itoa(displayNum)
	buf := make([]byte, 0, 10+len(number)+len(xauthProtocol)+len(cookie))
	buf = binary.BigEndian.AppendUint16(buf, xauthFamilyWild)
	for _, field := range [][]byte{nil, []byte(number), []byte(xauthProtocol), cookie} {
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(field)))
		buf = append(buf, field...)
	}
	return buf
}

// WriteXauthority creates path (exclusively, mode 0600, never following
// symlinks) holding a fresh random MIT-MAGIC-COOKIE-1 for display :99 and
// hands ownership to uid:gid; pass -1 to leave an ID unchanged. The cookie is
// never returned or logged.
//
// It must run while the process can still create files for the browser user,
// i.e. from PrepareRoot as root: after DropPrivileges the supervisor cannot
// write into the browser-owned runtime directory.
func WriteXauthority(path string, uid, gid int) error {
	cookie := make([]byte, xauthCookieBytes)
	if _, err := rand.Read(cookie); err != nil {
		return fmt.Errorf("xauthority: generate cookie: %w", err)
	}
	if err := auth.WritePrivateFile(path, encodeXauthority(displayNumber, cookie)); err != nil {
		return fmt.Errorf("xauthority: %w", err)
	}
	if err := os.Lchown(path, uid, gid); err != nil {
		return fmt.Errorf("xauthority: chown: %w", err)
	}
	return nil
}
