// This isolated study endpoint never modifies the machine's installed services.
package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pion/turn/v5"
)

func main() {
	public := flag.String("public", "", "Public IPv4 address of this study endpoint")
	flag.Parse()
	ip := net.ParseIP(*public).To4()
	if ip == nil {
		panic("public IPv4 required")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	issuer := base64.RawURLEncoding.EncodeToString(secret)
	udp, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		panic(err)
	}
	tcp, err := net.Listen("tcp4", udp.LocalAddr().String())
	if err != nil {
		panic(err)
	}
	const realm = "windshare.throughput-study"
	generator := &turn.RelayAddressGeneratorStatic{RelayAddress: ip, Address: "0.0.0.0"}
	server, err := turn.NewServer(turn.ServerConfig{
		Realm: realm,
		AuthHandler: func(attributes *turn.RequestAttributes) (string, []byte, bool) {
			expiry, _, found := strings.Cut(attributes.Username, ":")
			seconds, err := strconv.ParseInt(expiry, 10, 64)
			if !found || err != nil || time.Now().Unix() >= seconds {
				return "", nil, false
			}
			mac := hmac.New(sha1.New, []byte(issuer))
			_, _ = mac.Write([]byte(attributes.Username))
			password := base64.StdEncoding.EncodeToString(mac.Sum(nil))
			return attributes.Username, turn.GenerateAuthKey(attributes.Username, realm, password), true
		},
		PacketConnConfigs: []turn.PacketConnConfig{{PacketConn: udp, RelayAddressGenerator: generator}},
		ListenerConfigs:   []turn.ListenerConfig{{Listener: tcp, RelayAddressGenerator: generator}},
	})
	if err != nil {
		panic(err)
	}
	defer server.Close()
	port := udp.LocalAddr().(*net.UDPAddr).Port
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
		"url":          fmt.Sprintf("turn:%s:%d?transport=udp", *public, port),
		"tcpUrl":       fmt.Sprintf("turn:%s:%d?transport=tcp", *public, port),
		"sharedSecret": issuer,
	}); err != nil {
		panic(err)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
}
