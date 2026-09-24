package auth

import (
	"bytes"
	"image/png"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTPGenerate crée un secret et renvoie (secret base32, URL otpauth).
func TOTPGenerate(issuer, account string) (secret, url string, err error) {
	k, err := totp.Generate(totp.GenerateOpts{Issuer: issuer, AccountName: account})
	if err != nil {
		return "", "", err
	}
	return k.Secret(), k.URL(), nil
}

// TOTPValidate vérifie un code à 6 chiffres.
func TOTPValidate(secret, code string) bool {
	return totp.Validate(code, secret)
}

// TOTPQRPNG rend le QR code d'une URL otpauth.
func TOTPQRPNG(url string) ([]byte, error) {
	k, err := otp.NewKeyFromURL(url)
	if err != nil {
		return nil, err
	}
	img, err := k.Image(220, 220)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
