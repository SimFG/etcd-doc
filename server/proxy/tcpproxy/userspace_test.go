// Copyright 2016 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tcpproxy

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestUserspaceProxy(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	want := "hello proxy"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, want)
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}

	port, _ := strconv.ParseUint(u.Port(), 10, 16)
	p := TCPProxy{
		Listener:  l,
		Endpoints: []*net.SRV{{Target: u.Hostname(), Port: uint16(port)}},
	}
	go p.Run()
	defer p.Stop()

	u.Host = l.Addr().String()

	res, err := http.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	got, gerr := io.ReadAll(res.Body)
	res.Body.Close()
	if gerr != nil {
		t.Fatal(gerr)
	}

	if string(got) != want {
		t.Errorf("got = %s, want %s", got, want)
	}
}

// "127.0.0.1:0" 0端口可用于绑定到随机可用端口，但使用它来连接没有意义…
func TestUserspaceProxyPriority(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	backends := []struct {
		Payload  string
		Priority uint16
	}{
		{"hello proxy 2", 2},
		{"hello proxy 3", 3},
		{"hello proxy 1", 1},
	}

	var eps []*net.SRV
	var front *url.URL
	for index, b := range backends {
		backend := b
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, backend.Payload)
		}))
		defer ts.Close()

		fmt.Println(index, ts.URL)
		front, err = url.Parse(ts.URL)
		if err != nil {
			t.Fatal(err)
		}

		var port uint16
		fmt.Sscanf(front.Port(), "%d", &port)

		ep := &net.SRV{Target: front.Hostname(), Port: port, Priority: backend.Priority}
		eps = append(eps, ep)
	}

	p := TCPProxy{
		Listener:  l,
		Endpoints: eps,
	}
	go p.Run()
	defer p.Stop()

	front.Host = l.Addr().String()

	res, err := http.Get(front.String())
	if err != nil {
		t.Fatal(err)
	}
	got, gerr := io.ReadAll(res.Body)
	res.Body.Close()
	if gerr != nil {
		t.Fatal(gerr)
	}

	want := "hello proxy 1"
	if string(got) != want {
		t.Errorf("got = %s, want %s", got, want)
	}
}

func TestFormatAddr(t *testing.T) {
	addrs := []struct {
		host         string
		port         uint16
		expectedAddr string
	}{
		{
			"192.168.1.10",
			2379,
			"192.168.1.10:2379",
		},
		{
			"::1",
			2379,
			"[::1]:2379",
		},
		{
			"2001:db8::ff00:42:8329",
			80,
			"[2001:db8::ff00:42:8329]:80",
		},
	}

	for _, addr := range addrs {
		actualAddr := formatAddr(addr.host, addr.port)
		if actualAddr != addr.expectedAddr {
			t.Errorf("actualAddr: %s, expectedAddr: %s", actualAddr, addr.expectedAddr)
		}
	}
}
