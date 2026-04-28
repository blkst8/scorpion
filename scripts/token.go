//go:build ignore

package main

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	secret := []byte("change-me-real-token-secret")

	claims := jwt.MapClaims{
		"sub":     "12345", // client_id — must be a string, used by the server
		"user_id": 12345,
		"exp":     time.Now().Add(time.Hour * 24).Unix(),
		"iat":     time.Now().Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	tokenString, err := token.SignedString(secret)
	if err != nil {
		panic(err)
	}

	fmt.Println(tokenString)
}
