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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	agentdeploy "github.com/yawaflua/marusya-gemini/deploy/agent"
)

// fakeSpeaker is an SSH server that runs exec requests with the local sh,
// with /etc under root and stub rc-service/rc-update on PATH.
func fakeSpeaker(t *testing.T, password string) (addr, root string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	root = t.TempDir()
	bin := filepath.Join(root, "bin")
	for _, d := range []string{"etc/init.d", "etc/conf.d", "bin"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"rc-service", "rc-update"} {
		stub := "#!/bin/sh\necho " + name + " \"$@\" >> " + filepath.Join(root, "rc.log") + "\n"
		if err := os.WriteFile(filepath.Join(bin, name), []byte(stub), 0o755); err != nil {
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

func TestInstallAgent(t *testing.T) {
	addr, root := fakeSpeaker(t, "pw")
	host, port, _ := net.SplitHostPort(addr)
	req := installRequest{Host: host, Password: "pw", Server: "ws://10.0.0.5:8080/v1/stream"}
	req.Port, _ = strconv.Atoi(port)
	if err := req.validate(); err != nil {
		t.Fatal(err)
	}
	if req.User != "root" {
		t.Fatalf("user %q, want root by default", req.User)
	}

	res, err := installAgent(context.Background(), req, "tok'en")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, res.Output)
	}
	if !strings.HasPrefix(res.HostKey, "SHA256:") {
		t.Errorf("host key %q", res.HostKey)
	}
	init, _ := os.ReadFile(filepath.Join(root, "etc/init.d/marusya-agent"))
	if string(init) != agentdeploy.OpenRC {
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
	if log := readFile(t, filepath.Join(root, "rc.log")); !strings.Contains(log, "rc-update add marusya-agent default") || !strings.Contains(log, "rc-service marusya-agent restart") {
		t.Errorf("rc log:\n%s", log)
	}

	// A second install keeps the owner's settings and changes only the
	// server and the token.
	edited := strings.Replace(conf, "MARUSYA_DBUS=true", "MARUSYA_DBUS=false", 1)
	os.WriteFile(filepath.Join(root, "etc/conf.d/marusya-agent"), []byte(edited), 0o644)
	req.Server = "wss://marusya.example.com/v1/stream"
	if res, err := installAgent(context.Background(), req, "new"); err != nil {
		t.Fatalf("reinstall: %v\n%s", err, res.Output)
	}
	conf = readFile(t, filepath.Join(root, "etc/conf.d/marusya-agent"))
	for _, want := range []string{"\nMARUSYA_DBUS=false\n", "\nMARUSYA_SERVER=wss://marusya.example.com/v1/stream\n", "\nMARUSYA_AUTH_TOKEN=new\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("reinstalled config lacks %q:\n%s", want, conf)
		}
	}

	req.Password = "wrong"
	if _, err := installAgent(context.Background(), req, "new"); err == nil || !strings.Contains(err.Error(), "пароль") {
		t.Errorf("wrong password: %v", err)
	}
}

func TestInstallRequestValidate(t *testing.T) {
	for _, req := range []installRequest{
		{Host: "", Server: "ws://a:8080/v1/stream"},
		{Host: "1.2.3.4; rm", Server: "ws://a:8080/v1/stream"},
		{Host: "1.2.3.4", Server: "http://a:8080/v1/stream"},
		{Host: "1.2.3.4", Server: "ws://a:8080/v1/stream x"},
		{Host: "1.2.3.4", Port: 70000, Server: "ws://a:8080/v1/stream"},
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
