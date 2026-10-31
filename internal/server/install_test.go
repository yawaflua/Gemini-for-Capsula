// Copyright 2026 The Marusya-Gemini Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	agentdeploy "github.com/yawaflua/marusya-gemini/deploy/agent"
)

// speakerStubs stand in for the speaker's commands. They log their
// arguments to $INSTALL_ROOT/<name>.log. The spotifyd stub prints a sign-in
// link and waits until wget has "opened" the redirect.
var speakerStubs = map[string]string{
	"rc-service": `echo "$@" >> "$INSTALL_ROOT/rc.log"`,
	"rc-update":  `echo "$@" >> "$INSTALL_ROOT/rc.log"`,
	"apk":        `echo "$@" >> "$INSTALL_ROOT/apk.log"`,
	"dbus-send":  `true`,
	"addgroup":   `echo "$@" >> "$INSTALL_ROOT/addgroup.log"`,
	"wget":       `echo "$@" >> "$INSTALL_ROOT/wget.log"`,
	"spotifyd": `case "$1" in
--version) echo spotifyd 0.4.2 ;;
authenticate)
	echo "$@" > "$INSTALL_ROOT/auth.args"
	echo "Browse to: https://accounts.spotify.com/authorize?client_id=x"
	i=0
	while [ ! -f "$INSTALL_ROOT/wget.log" ]; do sleep 0.1; i=$((i+1)); [ $i -lt 100 ] || exit 1; done
	echo "Login successful!" ;;
esac`,
}

// fakeSpeaker is an SSH server that runs exec requests with the local sh,
// with /etc under root and [speakerStubs] on PATH.
func fakeSpeaker(t *testing.T, password string) (addr, root string) {
	t.Helper()
	for _, c := range []string{"sh", "tar", "awk"} {
		if _, err := exec.LookPath(c); err != nil {
			t.Skip("no " + c)
		}
	}
	root = t.TempDir()
	bin := filepath.Join(root, "bin")
	for _, d := range []string{"etc/init.d", "etc/conf.d", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range speakerStubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == "root" && string(pw) == password {
				return nil, nil
			}
			return nil, ssh.ErrNoAuth
		},
	}
	cfg.AddHostKey(signer)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go serveFakeSSH(nc, cfg, root, bin)
		}
	}()
	return l.Addr().String(), root
}

func serveFakeSSH(nc net.Conn, cfg *ssh.ServerConfig, root, bin string) {
	defer nc.Close()
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		ch, creqs, err := nch.Accept()
		if err != nil {
			return
		}
		go func() {
			defer ch.Close()
			for req := range creqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				n := binary.BigEndian.Uint32(req.Payload)
				script := string(req.Payload[4 : 4+n])
				req.Reply(true, nil)
				cmd := exec.Command("sh", "-c", script)
				cmd.Env = append(os.Environ(), "INSTALL_ROOT="+root, "PATH="+bin+":"+os.Getenv("PATH"))
				cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
				status := uint32(0)
				if err := cmd.Run(); err != nil {
					status = 1
				}
				ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, status))
				return
			}
		}()
	}
}

func fakeInstaller(t *testing.T, token string) *installer {
	t.Helper()
	// The AirPlay bundle: its install.sh records its arguments.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	script := []byte("#!/bin/sh\necho \"$@\" > \"$INSTALL_ROOT/airplay.args\"\necho INSTALL_PASS\n")
	tw.WriteHeader(&tar.Header{Name: "capsula-mini-airplay2-526959e/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "capsula-mini-airplay2-526959e/install.sh", Mode: 0o755, Size: int64(len(script))})
	tw.Write(script)
	tw.Close()
	gz.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(buf.Bytes()) }))
	t.Cleanup(srv.Close)
	return &installer{http: srv.Client(), airplayURL: srv.URL, token: token}
}

func speakerRequest(t *testing.T, addr, password string) installRequest {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	req := installRequest{Host: host, Password: password, Server: "ws://10.0.0.5:8080/v1/stream"}
	req.Port, _ = strconv.Atoi(port)
	if err := req.validate(); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestInstallAgent(t *testing.T) {
	addr, root := fakeSpeaker(t, "pw")
	req := speakerRequest(t, addr, "pw")
	if req.User != "root" || req.Name != "Маруся" {
		t.Fatalf("defaults: user %q, name %q", req.User, req.Name)
	}

	res, login, err := fakeInstaller(t, "tok'en").run(context.Background(), req)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, res.Output)
	}
	if login != nil {
		t.Error("a Spotify sign-in without Spotify")
	}
	if !strings.HasPrefix(res.HostKey, "SHA256:") {
		t.Errorf("host key %q", res.HostKey)
	}
	if init := readFile(t, filepath.Join(root, "etc/init.d/marusya-agent")); init != agentdeploy.OpenRC {
		t.Error("init script differs from deploy/agent/marusya-agent.openrc")
	}
	conf := readFile(t, filepath.Join(root, "etc/conf.d/marusya-agent"))
	for _, want := range []string{
		"\nMARUSYA_SERVER=ws://10.0.0.5:8080/v1/stream\n",
		"\nMARUSYA_AUTH_TOKEN='tok'\\''en'\n",
		"\nMARUSYA_DBUS=true\n", // the rest of the example config
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config lacks %q:\n%s", want, conf)
		}
	}
	if n := strings.Count(conf, "MARUSYA_SERVER="); n != 1 {
		t.Errorf("%d MARUSYA_SERVER lines", n)
	}
	// The token is read the way OpenRC does: by sourcing the file.
	out, err := exec.Command("sh", "-c", `. "$1"; printf %s "$MARUSYA_AUTH_TOKEN"`, "sh", filepath.Join(root, "etc/conf.d/marusya-agent")).Output()
	if err != nil || string(out) != "tok'en" {
		t.Errorf("sourced token %q, %v", out, err)
	}
	if log := readFile(t, filepath.Join(root, "rc.log")); !strings.Contains(log, "add marusya-agent default") || !strings.Contains(log, "marusya-agent restart") {
		t.Errorf("rc log:\n%s", log)
	}

	// A second install keeps the owner's settings and changes only the
	// server and the token.
	edited := strings.Replace(conf, "MARUSYA_DBUS=true", "MARUSYA_DBUS=false", 1)
	os.WriteFile(filepath.Join(root, "etc/conf.d/marusya-agent"), []byte(edited), 0o644)
	req.Server = "wss://marusya.example.com/v1/stream"
	if res, _, err := fakeInstaller(t, "new").run(context.Background(), req); err != nil {
		t.Fatalf("reinstall: %v\n%s", err, res.Output)
	}
	conf = readFile(t, filepath.Join(root, "etc/conf.d/marusya-agent"))
	for _, want := range []string{"\nMARUSYA_DBUS=false\n", "\nMARUSYA_SERVER=wss://marusya.example.com/v1/stream\n", "\nMARUSYA_AUTH_TOKEN=new\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("reinstalled config lacks %q:\n%s", want, conf)
		}
	}

	req.Password = "wrong"
	if _, _, err := fakeInstaller(t, "new").run(context.Background(), req); err == nil || !strings.Contains(err.Error(), "пароль") {
		t.Errorf("wrong password: %v", err)
	}
}

func TestInstallDBus(t *testing.T) {
	addr, root := fakeSpeaker(t, "pw")
	req := speakerRequest(t, addr, "pw")

	if res, _, err := fakeInstaller(t, "tok").run(context.Background(), req); err != nil {
		t.Fatalf("install: %v\n%s", err, res.Output)
	}
	if _, err := os.Stat(filepath.Join(root, "etc/dbus-1/system.d/ru.marusya.Agent.conf")); err == nil {
		t.Error("D-Bus policy installed without asking")
	}

	// The owner had turned the service off; asking for D-Bus turns it on.
	conf := filepath.Join(root, "etc/conf.d/marusya-agent")
	os.WriteFile(conf, []byte(strings.Replace(readFile(t, conf), "MARUSYA_DBUS=true", "MARUSYA_DBUS=false", 1)), 0o644)
	req.DBus = true
	res, _, err := fakeInstaller(t, "tok").run(context.Background(), req)
	if err != nil {
		t.Fatalf("install with D-Bus: %v\n%s", err, res.Output)
	}
	if got := readFile(t, filepath.Join(root, "etc/dbus-1/system.d/ru.marusya.Agent.conf")); got != agentdeploy.AgentDBus {
		t.Error("D-Bus policy differs from deploy/agent/ru.marusya.Agent.conf")
	}
	if got := readFile(t, filepath.Join(root, "addgroup.log")); got != "-S marusya\n" {
		t.Errorf("addgroup %q", got)
	}
	if got := readFile(t, conf); !strings.Contains(got, "\nMARUSYA_DBUS=true\n") || strings.Contains(got, "MARUSYA_DBUS=false") {
		t.Errorf("config:\n%s", got)
	}
	log := readFile(t, filepath.Join(root, "rc.log"))
	if !strings.Contains(log, "add dbus default") {
		t.Errorf("dbus not enabled:\n%s", log)
	}
	// The policy is in place before the agent restarts and asks for the name.
	if i, j := strings.LastIndex(log, "dbus status"), strings.LastIndex(log, "marusya-agent restart"); i < 0 || i > j {
		t.Errorf("agent restarted before D-Bus was set up:\n%s", log)
	}

	// The group exists now: a second run leaves it alone.
	os.WriteFile(filepath.Join(root, "etc/group"), []byte("root:x:0:\nmarusya:x:101:\n"), 0o644)
	if res, _, err := fakeInstaller(t, "tok").run(context.Background(), req); err != nil {
		t.Fatalf("reinstall: %v\n%s", err, res.Output)
	}
	if got := readFile(t, filepath.Join(root, "addgroup.log")); got != "-S marusya\n" {
		t.Errorf("addgroup ran again: %q", got)
	}
}

func TestInstallAirPlayAndSpotify(t *testing.T) {
	addr, root := fakeSpeaker(t, "pw")
	req := speakerRequest(t, addr, "pw")
	req.Name = "Кухня Маши"
	req.AirPlay, req.Spotify = true, true
	// The bundle's stock config: no metadata, shairport-sync's own volume.
	conf := filepath.Join(root, "etc/shairport-sync.conf")
	if err := os.WriteFile(conf, []byte("general = {\n\tname = \"Кухня Маши\";\n};\n\nalsa = {\n\toutput_device = \"default\";\n};\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, login, err := fakeInstaller(t, "tok").run(context.Background(), req)
	if err != nil {
		t.Fatalf("install: %v\n%s", err, res.Output)
	}
	if args := readFile(t, filepath.Join(root, "airplay.args")); args != "--upgrade --name Кухня Маши\n" {
		t.Errorf("AirPlay installer args %q", args)
	}
	if !strings.Contains(res.Output, "INSTALL_PASS") {
		t.Errorf("output lacks the AirPlay installer's:\n%s", res.Output)
	}
	got := readFile(t, conf)
	if !strings.Contains(got, "general = {\n\tignore_volume_control = \"yes\";\n\tname") ||
		!strings.Contains(got, `pipe_name = "/tmp/shairport-sync-metadata";`) {
		t.Errorf("shairport-sync.conf:\n%s", got)
	}
	if rc := readFile(t, filepath.Join(root, "rc.log")); !strings.Contains(rc, "shairport-sync restart") {
		t.Errorf("shairport-sync not restarted:\n%s", rc)
	}

	if _, err := os.Stat(filepath.Join(root, "apk.log")); err == nil {
		t.Error("apk ran although spotifyd is there")
	}
	if got := readFile(t, filepath.Join(root, "etc/init.d/spotifyd")); got != agentdeploy.SpotifydOpenRC {
		t.Error("spotifyd init script differs")
	}
	if got := readFile(t, filepath.Join(root, "etc/spotifyd.conf")); !strings.Contains(got, `device_name = "Кухня Маши"`) || !strings.Contains(got, `dbus_type = "system"`) {
		t.Errorf("spotifyd.conf:\n%s", got)
	}
	if got := readFile(t, filepath.Join(root, "etc/dbus-1/system.d/spotifyd.conf")); got != agentdeploy.SpotifydDBus {
		t.Error("D-Bus policy differs")
	}

	if login == nil {
		t.Fatal("no Spotify sign-in")
	}
	defer login.close()
	if !strings.HasPrefix(login.url, "https://accounts.spotify.com/authorize") {
		t.Errorf("sign-in link %q", login.url)
	}
	if _, err := login.finish(context.Background(), "http://localhost:9999/elsewhere?code=x"); err == nil {
		t.Error("a redirect to another path was accepted")
	}
	out, err := login.finish(context.Background(), "http://127.0.0.1:8000/login?code=AQ%20B&state=s1&evil=';reboot'")
	if err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	if got := readFile(t, filepath.Join(root, "wget.log")); got != "-q -O /dev/null -T 15 http://127.0.0.1:8000/login?code=AQ+B&state=s1\n" {
		t.Errorf("wget %q", got)
	}
	if !strings.Contains(out, "Login successful") {
		t.Errorf("output:\n%s", out)
	}
	if log := readFile(t, filepath.Join(root, "rc.log")); strings.Count(log, "spotifyd restart") != 2 {
		t.Errorf("spotifyd restarted %d times, want after install and after sign-in:\n%s", strings.Count(log, "spotifyd restart"), log)
	}
}

func TestSpotifyLoginsExpire(t *testing.T) {
	var ls spotifyLogins
	l := &spotifyLogin{}
	id := ls.add(l)
	if ls.take("nope") != nil {
		t.Error("took an unknown sign-in")
	}
	if ls.take(id) != l || ls.take(id) != nil {
		t.Error("a sign-in is taken once")
	}
}

func TestInstallRequestValidate(t *testing.T) {
	for _, req := range []installRequest{
		{Host: "", Server: "ws://a:8080/v1/stream"},
		{Host: "1.2.3.4; rm", Server: "ws://a:8080/v1/stream"},
		{Host: "1.2.3.4", Server: "http://a:8080/v1/stream"},
		{Host: "1.2.3.4", Server: "ws://a:8080/v1/stream x"},
		{Host: "1.2.3.4", Port: 70000, Server: "ws://a:8080/v1/stream"},
		{Host: "1.2.3.4", Server: "ws://a:8080/v1/stream", Name: `Ку"хня`},
		{Host: "1.2.3.4", Server: "ws://a:8080/v1/stream", Name: strings.Repeat("я", 26)},
	} {
		if err := req.validate(); err == nil {
			t.Errorf("%+v: no error", req)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
