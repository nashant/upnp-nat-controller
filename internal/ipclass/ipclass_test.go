package ipclass

import "testing"

func TestClassifyExternalIP_PrivateCGNATPublic(t *testing.T) {
	cases := map[string]Class{
		"":                Empty,
		"not-an-ip":       Empty,
		"10.0.0.1":        Private,
		"172.16.1.2":      Private,
		"172.31.255.255":  Private,
		"192.168.1.10":    Private,
		"100.64.0.1":      CGNAT,
		"100.127.255.254": CGNAT,
		"100.128.0.1":     Public,
		"172.32.0.1":      Public,
		"81.2.69.142":     Public,
		"0.0.0.0":         Empty,
		"127.0.0.1":       Private,
		"169.254.1.1":     Private,
	}
	for ip, want := range cases {
		if got := ClassifyExternalIP(ip); got != want {
			t.Errorf("ClassifyExternalIP(%q) = %v, want %v", ip, got, want)
		}
	}
}

func TestClass_IsPublic(t *testing.T) {
	if !Public.IsPublic() || Private.IsPublic() || CGNAT.IsPublic() || Empty.IsPublic() {
		t.Fatal("IsPublic wrong")
	}
}
