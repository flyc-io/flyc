package auth

import "testing"

func TestPasswordRoundTrip(t *testing.T) {
	h := HashPassword("correct horse battery staple")
	if !VerifyPassword(h, "correct horse battery staple") {
		t.Fatal("mot de passe valide refusé")
	}
	if VerifyPassword(h, "wrong") || VerifyPassword("garbage", "x") || VerifyPassword("", "x") {
		t.Fatal("mot de passe invalide accepté")
	}
}

func TestTOTP(t *testing.T) {
	secret, url, err := TOTPGenerate("Flyc", "admin@localhost")
	if err != nil || secret == "" || url == "" {
		t.Fatal(err)
	}
	if TOTPValidate(secret, "000000") && TOTPValidate(secret, "123456") {
		t.Fatal("deux codes arbitraires acceptés")
	}
	if png, err := TOTPQRPNG(url); err != nil || len(png) < 100 {
		t.Fatalf("QR: %v", err)
	}
}
