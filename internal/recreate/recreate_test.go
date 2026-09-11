package recreate

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const oldJSON = `{
 "Id": "0123456789abcdef0123",
 "Config": {
   "Hostname": "0123456789ab",
   "Image": "nginx:1.27",
   "Env": ["PATH=/usr/bin", "NGINX_VERSION=1.27.1", "FOO=bar"],
   "Cmd": ["nginx", "-g", "daemon off;"],
   "Entrypoint": ["/docker-entrypoint.sh"],
   "Labels": {"maintainer": "NGINX", "bosun.enable": "true", "com.docker.compose.service": "web"},
   "ExposedPorts": {"80/tcp": {}}
 },
 "HostConfig": {
   "Binds": ["/srv/site:/usr/share/nginx/html:ro"],
   "NetworkMode": "web_default",
   "Privileged": false,
   "CapAdd": null,
   "RestartPolicy": {"Name": "unless-stopped"}
 },
 "NetworkSettings": {"Networks": {
   "web_default": {"Aliases": ["web", "0123456789ab"], "IPAMConfig": null, "NetworkID": "n1", "EndpointID": "e1", "IPAddress": "172.18.0.2"},
   "proxy": {"Aliases": ["web"], "IPAMConfig": {"IPv4Address": "10.0.0.5"}}
 }},
 "Mounts": [
   {"Type": "bind", "Source": "/srv/site", "Destination": "/usr/share/nginx/html"},
   {"Type": "volume", "Name": "3f9a", "Destination": "/var/cache/nginx"}
 ]
}`

const imgJSON = `{
 "Env": ["PATH=/usr/bin", "NGINX_VERSION=1.27.1"],
 "Cmd": ["nginx", "-g", "daemon off;"],
 "Entrypoint": ["/docker-entrypoint.sh"],
 "Labels": {"maintainer": "NGINX"},
 "ExposedPorts": {"80/tcp": {}}
}`

func build(t *testing.T, old string) map[string]any {
	t.Helper()
	b, err := Build(json.RawMessage(old), json.RawMessage(imgJSON), "nginx:1.28")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildKeepsOnlyUserConfig(t *testing.T) {
	m := build(t, oldJSON)
	if m["Image"] != "nginx:1.28" {
		t.Errorf("Image = %v", m["Image"])
	}
	if !reflect.DeepEqual(m["Env"], []any{"FOO=bar"}) {
		t.Errorf("Env = %v, want only the user's FOO=bar", m["Env"])
	}
	want := map[string]any{"bosun.enable": "true", "com.docker.compose.service": "web"}
	if !reflect.DeepEqual(m["Labels"], want) {
		t.Errorf("Labels = %v, want %v", m["Labels"], want)
	}
	for _, k := range []string{"Cmd", "Entrypoint", "ExposedPorts", "Hostname"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s was copied from the old image; the new image must supply it", k)
		}
	}
}

func TestBuildKeepsUserCmd(t *testing.T) {
	old := strings.Replace(oldJSON, `"Cmd": ["nginx", "-g", "daemon off;"]`, `"Cmd": ["nginx", "-T"]`, 1)
	m := build(t, old)
	if !reflect.DeepEqual(m["Cmd"], []any{"nginx", "-T"}) {
		t.Errorf("Cmd = %v, want the user's override", m["Cmd"])
	}
}

func TestBuildKeepsCustomHostname(t *testing.T) {
	old := strings.Replace(oldJSON, `"Hostname": "0123456789ab"`, `"Hostname": "web01"`, 1)
	if m := build(t, old); m["Hostname"] != "web01" {
		t.Errorf("Hostname = %v, want web01", m["Hostname"])
	}
}

// The new container may gain nothing on the host that the old one did not
// have. HostConfig is copied, and the only addition allowed is the old
// container's own anonymous volumes.
func TestBuildHostConfigOnlyGainsOldVolumes(t *testing.T) {
	var old struct{ HostConfig map[string]any }
	json.Unmarshal([]byte(oldJSON), &old)
	hc := build(t, oldJSON)["HostConfig"].(map[string]any)

	mounts := hc["Mounts"].([]any)
	delete(hc, "Mounts")
	if !reflect.DeepEqual(hc, old.HostConfig) {
		t.Errorf("HostConfig changed:\n got  %v\n want %v", hc, old.HostConfig)
	}
	want := []any{map[string]any{"Type": "volume", "Source": "3f9a", "Target": "/var/cache/nginx"}}
	if !reflect.DeepEqual(mounts, want) {
		t.Errorf("Mounts = %v, want only the anonymous volume %v", mounts, want)
	}
}

func TestBuildNamedVolumesAreNotAddedTwice(t *testing.T) {
	old := strings.Replace(oldJSON, `"Binds": ["/srv/site:/usr/share/nginx/html:ro"]`,
		`"Binds": ["/srv/site:/usr/share/nginx/html:ro", "3f9a:/var/cache/nginx"]`, 1)
	hc := build(t, old)["HostConfig"].(map[string]any)
	if _, ok := hc["Mounts"]; ok {
		t.Errorf("volume already in Binds was added again: %v", hc["Mounts"])
	}
}

func TestBuildNetworks(t *testing.T) {
	m := build(t, oldJSON)
	eps := m["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)
	web := eps["web_default"].(map[string]any)
	if !reflect.DeepEqual(web["Aliases"], []any{"web"}) {
		t.Errorf("web_default aliases = %v, want [web] without the old ID", web["Aliases"])
	}
	for _, k := range []string{"NetworkID", "EndpointID", "IPAddress"} {
		if _, ok := web[k]; ok {
			t.Errorf("%s copied; Docker must assign it", k)
		}
	}
	proxy := eps["proxy"].(map[string]any)
	if !reflect.DeepEqual(proxy["IPAMConfig"], map[string]any{"IPv4Address": "10.0.0.5"}) {
		t.Errorf("proxy lost its static IP: %v", proxy)
	}
}

func TestBuildHostNetworkHasNoEndpoints(t *testing.T) {
	old := strings.Replace(oldJSON, `"NetworkMode": "web_default"`, `"NetworkMode": "host"`, 1)
	if _, ok := build(t, old)["NetworkingConfig"]; ok {
		t.Error("host network mode must not get NetworkingConfig")
	}
}
