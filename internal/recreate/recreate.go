// Package recreate builds the Docker create request for a container's
// replacement. Only the image changes. Everything else comes from the old
// container. This is where "the gate can only swap images" is made true.
package recreate

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
)

type mount struct{ Type, Name, Destination string }

type oldContainer struct {
	ID              string `json:"Id"`
	Config          map[string]json.RawMessage
	HostConfig      map[string]json.RawMessage
	NetworkSettings struct {
		Networks map[string]map[string]json.RawMessage
	}
	Mounts []mount
}

// Build returns the create body for a copy of old that runs newRef.
// old is the full inspect JSON of the old container. imgConfig is the Config
// of the old container's image. Values that only repeat image defaults are
// left out, so the new image can supply its own.
func Build(old, imgConfig json.RawMessage, newRef string) (json.RawMessage, error) {
	var c oldContainer
	if err := json.Unmarshal(old, &c); err != nil {
		return nil, err
	}
	var img map[string]json.RawMessage
	if err := unmarshalOpt(imgConfig, &img); err != nil {
		return nil, err
	}
	body := map[string]any{}
	for k, v := range c.Config {
		keep, err := userValue(k, v, img[k], c.ID)
		if err != nil {
			return nil, err
		}
		if keep != nil {
			body[k] = keep
		}
	}
	body["Image"] = newRef
	hc, err := withAnonVolumes(c.HostConfig, c.Mounts)
	if err != nil {
		return nil, err
	}
	body["HostConfig"] = hc
	if nc := networking(c.HostConfig, c.NetworkSettings.Networks, c.ID); nc != nil {
		body["NetworkingConfig"] = nc
	}
	return json.Marshal(body)
}

// userValue returns the part of a Config value the user set, or nil when the
// value only repeats the image default.
func userValue(key string, v, img json.RawMessage, id string) (json.RawMessage, error) {
	switch key {
	case "Image":
		return nil, nil
	case "Hostname":
		// Docker sets the hostname to the short container ID by default.
		// Copying it would name the new container after the old one.
		var h string
		if err := json.Unmarshal(v, &h); err != nil {
			return nil, err
		}
		if h == "" || (len(id) >= 12 && h == id[:12]) {
			return nil, nil
		}
		return v, nil
	case "Env":
		var got, def []string
		if err := unmarshalOpt(v, &got); err != nil {
			return nil, err
		}
		if err := unmarshalOpt(img, &def); err != nil {
			return nil, err
		}
		got = slices.DeleteFunc(got, func(e string) bool { return slices.Contains(def, e) })
		if len(got) == 0 {
			return nil, nil
		}
		return json.Marshal(got)
	case "Labels", "ExposedPorts", "Volumes":
		var got, def map[string]json.RawMessage
		if err := unmarshalOpt(v, &got); err != nil {
			return nil, err
		}
		if err := unmarshalOpt(img, &def); err != nil {
			return nil, err
		}
		maps.DeleteFunc(got, func(k string, val json.RawMessage) bool {
			d, ok := def[k]
			return ok && same(val, d)
		})
		if len(got) == 0 {
			return nil, nil
		}
		return json.Marshal(got)
	}
	if same(v, img) {
		return nil, nil
	}
	return v, nil
}

// withAnonVolumes adds the old container's anonymous volumes (from the image's
// VOLUME lines) to HostConfig.Mounts by name, so their data carries over.
// A volume in HostConfig.Mounts with no Source (--mount type=volume,dst=/data)
// is anonymous too; it gets the old volume's name. Named volumes and binds are
// already in HostConfig.
func withAnonVolumes(hc map[string]json.RawMessage, mounts []mount) (map[string]json.RawMessage, error) {
	covered := map[string]bool{}
	var binds []string
	if err := unmarshalOpt(hc["Binds"], &binds); err != nil {
		return nil, err
	}
	for _, b := range binds {
		if p := strings.Split(b, ":"); len(p) >= 2 {
			covered[p[1]] = true
		}
	}
	var ms []map[string]any
	if err := unmarshalOpt(hc["Mounts"], &ms); err != nil {
		return nil, err
	}
	changed := false
	for _, m := range ms {
		t, ok := m["Target"].(string)
		if !ok {
			continue
		}
		covered[t] = true
		if src, _ := m["Source"].(string); m["Type"] == "volume" && src == "" {
			for _, o := range mounts {
				if o.Type == "volume" && o.Name != "" && o.Destination == t {
					m["Source"] = o.Name
					changed = true
				}
			}
		}
	}
	n := len(ms)
	for _, m := range mounts {
		if m.Type == "volume" && m.Name != "" && !covered[m.Destination] {
			ms = append(ms, map[string]any{"Type": "volume", "Source": m.Name, "Target": m.Destination})
		}
	}
	if len(ms) == n && !changed {
		return hc, nil
	}
	b, err := json.Marshal(ms)
	if err != nil {
		return nil, err
	}
	out := maps.Clone(hc)
	out["Mounts"] = b
	return out, nil
}

// networking reconnects the new container to every network the old one was
// on, with its aliases and static IPs. Docker assigns the rest.
func networking(hc map[string]json.RawMessage, nets map[string]map[string]json.RawMessage, id string) map[string]any {
	var mode string
	_ = unmarshalOpt(hc["NetworkMode"], &mode)
	if mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") || len(nets) == 0 {
		return nil
	}
	eps := map[string]any{}
	for name, ep := range nets {
		e := map[string]any{}
		for _, k := range []string{"IPAMConfig", "Links", "DriverOpts"} {
			if v, ok := ep[k]; ok && string(v) != "null" {
				e[k] = v
			}
		}
		var aliases []string
		_ = unmarshalOpt(ep["Aliases"], &aliases)
		aliases = slices.DeleteFunc(aliases, func(a string) bool { return len(id) >= 12 && a == id[:12] })
		if len(aliases) > 0 {
			e["Aliases"] = aliases
		}
		eps[name] = e
	}
	return map[string]any{"EndpointsConfig": eps}
}

func unmarshalOpt(b json.RawMessage, v any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

func same(a, b json.RawMessage) bool {
	if len(a) == 0 {
		a = json.RawMessage("null")
	}
	if len(b) == 0 {
		b = json.RawMessage("null")
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
