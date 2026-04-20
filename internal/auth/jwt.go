// Package auth provides JWT ticket generation and validation for Scorpion.
package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/blkst8/scorpion/internal/config"
)

// TicketClaims holds the JWT claims for a pre-auth ticket.
type TicketClaims struct {
	IP string `json:"ip"`
	jwt.RegisteredClaims
}

// GenerateTicket creates a signed JWT ticket for the given client and IP.
func GenerateTicket(cfg config.Auth, clientID, clientIP string) (signedToken string, jti string, err error) {
	jti = uuid.NewString()
	now := time.Now()

	claims := TicketClaims{
		IP: clientIP,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   clientID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(cfg.TicketTTL)),
			ID:        jti,
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(cfg.TicketSecret))
	if err != nil {
		return "", "", fmt.Errorf("failed to sign ticket: %w", err)
	}

	return signed, jti, nil
}

// ValidateTicket parses and validates a JWT ticket string.
func ValidateTicket(cfg config.Auth, tokenStr string) (*TicketClaims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &TicketClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(cfg.TicketSecret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("invalid ticket: %w", err)
	}

	claims, ok := token.Claims.(*TicketClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid ticket claims")
	}

	return claims, nil
}

// ValidateRealToken validates the real Bearer token from an Authorization header
// and extracts the client_id (sub). This is a HMAC-signed JWT validation.
func ValidateRealToken(cfg config.Auth, tokenStr string) (string, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(cfg.TokenSecret), nil
	})
	if err != nil {
		return "", fmt.Errorf("invalid token: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return "", fmt.Errorf("invalid token claims")
	}

	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return "", fmt.Errorf("missing subject in token")
	}

	return sub, nil
}
