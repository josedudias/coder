package license

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"golang.org/x/xerrors"

	"cdr.dev/slog"

	"github.com/coder/coder/coderd/database"
	"github.com/coder/coder/codersdk"
)

// Entitlements processes licenses to return whether features are enabled or not.
func Entitlements(
	ctx context.Context,
	db database.Store,
	logger slog.Logger,
	replicaCount int,
	gitAuthCount int,
	keys map[string]ed25519.PublicKey,
	enablements map[codersdk.FeatureName]bool,
) (codersdk.Entitlements, error) {
	now := time.Now()
	// Default all entitlements to be disabled.
	entitlements := codersdk.Entitlements{
		Features: map[codersdk.FeatureName]codersdk.Feature{},
		Warnings: []string{},
		Errors:   []string{},
	}
	for _, featureName := range codersdk.FeatureNames {
		entitlements.Features[featureName] = codersdk.Feature{
			Entitlement: codersdk.EntitlementNotEntitled,
			Enabled:     enablements[featureName],
		}
	}

	licenses, err := db.GetUnexpiredLicenses(ctx)
	if err != nil {
		return entitlements, err
	}

	activeUserCount, err := db.GetActiveUserCount(ctx)
	if err != nil {
		return entitlements, xerrors.Errorf("query active user count: %w", err)
	}

	allFeatures := false

	// Here we loop through licenses to detect enabled features.
	for _, l := range licenses {
		claims, err := ParseClaims(l.JWT, keys)
		if err != nil {
			logger.Debug(ctx, "skipping invalid license",
				slog.F("id", l.ID), slog.Error(err))
			continue
		}
		entitlements.HasLicense = true
		entitlement := codersdk.EntitlementEntitled
		entitlements.Trial = claims.Trial
		if now.After(claims.LicenseExpires.Time) {
			// if the grace period were over, the validation fails, so if we are after
			// LicenseExpires we must be in grace period.
			entitlement = codersdk.EntitlementGracePeriod
		}
		for featureName, featureValue := range claims.Features {
			// Can this be negative?
			if featureValue <= 0 {
				continue
			}

			switch featureName {
			// User limit has special treatment as our only non-boolean feature.
			case codersdk.FeatureUserLimit:
				limit := featureValue
				priorLimit := entitlements.Features[codersdk.FeatureUserLimit]
				if priorLimit.Limit != nil && *priorLimit.Limit > limit {
					limit = *priorLimit.Limit
				}
				entitlements.Features[codersdk.FeatureUserLimit] = codersdk.Feature{
					Enabled:     true,
					Entitlement: entitlement,
					Limit:       &limit,
					Actual:      &activeUserCount,
				}
			default:
				entitlements.Features[featureName] = codersdk.Feature{
					Entitlement: entitlement,
					Enabled:     enablements[featureName] || featureName.AlwaysEnable(),
				}
			}
		}

		if claims.AllFeatures {
			allFeatures = true
		}
		entitlements.RequireTelemetry = entitlements.RequireTelemetry || claims.RequireTelemetry
	}

	if allFeatures {
		for _, featureName := range codersdk.FeatureNames {
			// No user limit!
			if featureName == codersdk.FeatureUserLimit {
				continue
			}
			feature := entitlements.Features[featureName]
			feature.Entitlement = codersdk.EntitlementEntitled
			entitlements.Features[featureName] = feature
		}
	}

	if entitlements.HasLicense {
		userLimit := entitlements.Features[codersdk.FeatureUserLimit].Limit
		if userLimit != nil && activeUserCount > *userLimit {
			entitlements.Warnings = append(entitlements.Warnings, fmt.Sprintf(
				"Your deployment has %d active users but is only licensed for %d.",
				activeUserCount, *userLimit))
		}

		for _, featureName := range codersdk.FeatureNames {
			// The user limit has it's own warnings!
			if featureName == codersdk.FeatureUserLimit {
				continue
			}
			// High availability has it's own warnings based on replica count!
			if featureName == codersdk.FeatureHighAvailability {
				continue
			}
			// Multiple Git auth has it's own warnings based on the number configured!
			if featureName == codersdk.FeatureMultipleGitAuth {
				continue
			}
			feature := entitlements.Features[featureName]
			if !feature.Enabled {
				continue
			}
			niceName := featureName.Humanize()
			switch feature.Entitlement {
			case codersdk.EntitlementNotEntitled:
				entitlements.Warnings = append(entitlements.Warnings,
					fmt.Sprintf("%s is enabled but your license is not entitled to this feature.", niceName))
			case codersdk.EntitlementGracePeriod:
				entitlements.Warnings = append(entitlements.Warnings,
					fmt.Sprintf("%s is enabled but your license for this feature is expired.", niceName))
			default:
			}
		}
	}

	if replicaCount > 1 {
		feature := entitlements.Features[codersdk.FeatureHighAvailability]

		switch feature.Entitlement {
		case codersdk.EntitlementNotEntitled:
			if entitlements.HasLicense {
				entitlements.Errors = append(entitlements.Errors,
					"You have multiple replicas but your license is not entitled to high availability. You will be unable to connect to workspaces.")
			} else {
				entitlements.Errors = append(entitlements.Errors,
					"You have multiple replicas but high availability is an Enterprise feature. You will be unable to connect to workspaces.")
			}
		case codersdk.EntitlementGracePeriod:
			entitlements.Warnings = append(entitlements.Warnings,
				"You have multiple replicas but your license for high availability is expired. Reduce to one replica or workspace connections will stop working.")
		}
	}

	if gitAuthCount > 1 {
		feature := entitlements.Features[codersdk.FeatureMultipleGitAuth]

		switch feature.Entitlement {
		case codersdk.EntitlementNotEntitled:
			if entitlements.HasLicense {
				entitlements.Errors = append(entitlements.Errors,
					"You have multiple Git authorizations configured but your license is limited at one.",
				)
			} else {
				entitlements.Errors = append(entitlements.Errors,
					"You have multiple Git authorizations configured but this is an Enterprise feature. Reduce to one.",
				)
			}
		case codersdk.EntitlementGracePeriod:
			entitlements.Warnings = append(entitlements.Warnings,
				"You have multiple Git authorizations configured but your license is expired. Reduce to one.",
			)
		}
	}

	for _, featureName := range codersdk.FeatureNames {
		feature := entitlements.Features[featureName]
		if feature.Entitlement == codersdk.EntitlementNotEntitled {
			feature.Enabled = false
			entitlements.Features[featureName] = feature
		}
	}

	return entitlements, nil
}

const (
	CurrentVersion        = 3
	HeaderKeyID           = "kid"
	AccountTypeSalesforce = "salesforce"
	VersionClaim          = "version"
)

var (
	ValidMethods = []string{"EdDSA"}

	ErrInvalidVersion        = xerrors.New("license must be version 3")
	ErrMissingKeyID          = xerrors.Errorf("JOSE header must contain %s", HeaderKeyID)
	ErrMissingLicenseExpires = xerrors.New("license missing license_expires")
)

type Features map[codersdk.FeatureName]int64

type Claims struct {
	jwt.RegisteredClaims
	// LicenseExpires is the end of the legit license term, and the start of the grace period, if
	// there is one.  The standard JWT claim "exp" (ExpiresAt in jwt.RegisteredClaims, above) is
	// the end of the grace period (identical to LicenseExpires if there is no grace period).
	// The reason we use the standard claim for the end of the grace period is that we want JWT
	// processing libraries to consider the token "valid" until then.
	LicenseExpires   *jwt.NumericDate `json:"license_expires,omitempty"`
	AccountType      string           `json:"account_type,omitempty"`
	AccountID        string           `json:"account_id,omitempty"`
	Trial            bool             `json:"trial"`
	AllFeatures      bool             `json:"all_features"`
	Version          uint64           `json:"version"`
	Features         Features         `json:"features"`
	RequireTelemetry bool             `json:"require_telemetry,omitempty"`
}

// ParseRaw consumes a license and returns the claims.
func ParseRaw(l string, keys map[string]ed25519.PublicKey) (jwt.MapClaims, error) {
	tok, err := jwt.Parse(
		l,
		keyFunc(keys),
		jwt.WithValidMethods(ValidMethods),
	)
	if err != nil {
		return nil, err
	}
	if claims, ok := tok.Claims.(jwt.MapClaims); ok && tok.Valid {
		version, ok := claims[VersionClaim].(float64)
		if !ok {
			return nil, ErrInvalidVersion
		}
		if int64(version) != CurrentVersion {
			return nil, ErrInvalidVersion
		}
		return claims, nil
	}
	return nil, xerrors.New("unable to parse Claims")
}

// ParseClaims validates a database.License record, and if valid, returns the claims.  If
// unparsable or invalid, it returns an error
func ParseClaims(rawJWT string, keys map[string]ed25519.PublicKey) (*Claims, error) {
	tok, err := jwt.ParseWithClaims(
		rawJWT,
		&Claims{},
		keyFunc(keys),
		jwt.WithValidMethods(ValidMethods),
	)
	if err != nil {
		return nil, err
	}
	if claims, ok := tok.Claims.(*Claims); ok && tok.Valid {
		if claims.Version != uint64(CurrentVersion) {
			return nil, ErrInvalidVersion
		}
		if claims.LicenseExpires == nil {
			return nil, ErrMissingLicenseExpires
		}
		return claims, nil
	}
	return nil, xerrors.New("unable to parse Claims")
}

func keyFunc(keys map[string]ed25519.PublicKey) func(*jwt.Token) (interface{}, error) {
	return func(j *jwt.Token) (interface{}, error) {
		keyID, ok := j.Header[HeaderKeyID].(string)
		if !ok {
			return nil, ErrMissingKeyID
		}
		k, ok := keys[keyID]
		if !ok {
			return nil, xerrors.Errorf("no key with ID %s", keyID)
		}
		return k, nil
	}
}
