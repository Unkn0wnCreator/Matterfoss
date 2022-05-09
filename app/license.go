// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/pkg/errors"

	"github.com/cjdelisle/matterfoss-server/v6/jobs"
	"github.com/cjdelisle/matterfoss-server/v6/model"
	"github.com/cjdelisle/matterfoss-server/v6/shared/mlog"
	"github.com/cjdelisle/matterfoss-server/v6/store"
	"github.com/cjdelisle/matterfoss-server/v6/utils"
)

const (
	LicenseEnv                = "MM_LICENSE"
	LicenseRenewalURL         = "https://customers.matterfoss.org/subscribe/renew"
	JWTDefaultTokenExpiration = 7 * 24 * time.Hour // 7 days of expiration
)

var RequestTrialURL = "https://customers.matterfoss.org/api/v1/trials"

// licenseWrapper is an adapter struct that only exposes the
// config related functionality to be passed down to other products.
type licenseWrapper struct {
	srv *Server
}

func (w *licenseWrapper) Name() ServiceKey {
	return LicenseKey
}

func (w *licenseWrapper) GetLicense() *model.License {
	return w.srv.License()
}

func (w *licenseWrapper) RequestTrialLicense(requesterID string, users int, termsAccepted bool, receiveEmailsAccepted bool) *model.AppError {
	if *w.srv.Config().ExperimentalSettings.RestrictSystemAdmin {
		return model.NewAppError("RequestTrialLicense", "api.restricted_system_admin", nil, "", http.StatusForbidden)
	}

	if !termsAccepted {
		return model.NewAppError("RequestTrialLicense", "api.license.request-trial.bad-request.terms-not-accepted", nil, "", http.StatusBadRequest)
	}

	if users == 0 {
		return model.NewAppError("RequestTrialLicense", "api.license.request-trial.bad-request", nil, "", http.StatusBadRequest)
	}

	requester, err := w.srv.userService.GetUser(requesterID)
	if err != nil {
		var nfErr *store.ErrNotFound
		switch {
		case errors.As(err, &nfErr):
			return model.NewAppError("RequestTrialLicense", MissingAccountError, nil, nfErr.Error(), http.StatusNotFound)
		default:
			return model.NewAppError("RequestTrialLicense", "app.user.get_by_username.app_error", nil, err.Error(), http.StatusInternalServerError)
		}
	}

	if *w.srv.Config().ServiceSettings.SiteURL == "" {
		return model.NewAppError("RequestTrialLicense", "api.license.request_trial_license.no-site-url.app_error", nil, "", http.StatusBadRequest)
	}

	trialLicenseRequest := &model.TrialLicenseRequest{
		ServerID:              w.srv.TelemetryId(),
		Name:                  requester.GetDisplayName(model.ShowFullName),
		Email:                 requester.Email,
		SiteName:              *w.srv.Config().TeamSettings.SiteName,
		SiteURL:               *w.srv.Config().ServiceSettings.SiteURL,
		Users:                 users,
		TermsAccepted:         termsAccepted,
		ReceiveEmailsAccepted: receiveEmailsAccepted,
	}

	return w.srv.RequestTrialLicense(trialLicenseRequest)
}

// JWTClaims custom JWT claims with the needed information for the
// renewal process
type JWTClaims struct {
	LicenseID   string `json:"license_id"`
	ActiveUsers int64  `json:"active_users"`
	jwt.StandardClaims
}

func (s *Server) LoadLicense() {
	// ENV var overrides all other sources of license.
	licenseStr := os.Getenv(LicenseEnv)
	if licenseStr != "" {
		license, err := utils.LicenseValidator.LicenseFromBytes([]byte(licenseStr))
		if err != nil {
			mlog.Error("Failed to read license set in environment.", mlog.Err(err))
			return
		}

		// skip the restrictions if license is a sanctioned trial
		if !license.IsSanctionedTrial() && license.IsTrialLicense() {
			canStartTrialLicense, err := s.LicenseManager.CanStartTrial()
			if err != nil {
				mlog.Error("Failed to validate trial eligibility.", mlog.Err(err))
				return
			}

			if !canStartTrialLicense {
				mlog.Info("Cannot start trial multiple times.")
				return
			}
		}

		if s.ValidateAndSetLicenseBytes([]byte(licenseStr)) {
			mlog.Info("License key from ENV is valid, unlocking enterprise features.")
		}
		return
	}

	// __MATTERFOSS__: Enable all OSS features for everyone
	f := model.Features{}
	f.SetDefaults()
	s.SetLicense(&model.License{
		Id:        "Anything that prevents you from being friendly, a good neighbour, is a terror tactic. -rms",
		IssuedAt:  0,
		ExpiresAt: 0x7fffffffffffffff,
		Customer:  &model.Customer{},
		Features:  &f,
	})
	mlog.Info("All features are enabled, do something good for someone today.")
}

func (s *Server) SaveLicense(licenseBytes []byte) (*model.License, *model.AppError) {
	success, licenseStr := utils.LicenseValidator.ValidateLicense(licenseBytes)
	if !success {
		return nil, model.NewAppError("addLicense", model.InvalidLicenseError, nil, "", http.StatusBadRequest)
	}

	var license model.License
	if jsonErr := json.Unmarshal([]byte(licenseStr), &license); jsonErr != nil {
		return nil, model.NewAppError("addLicense", "api.unmarshal_error", nil, jsonErr.Error(), http.StatusInternalServerError)
	}

	uniqueUserCount, err := s.Store.User().Count(model.UserCountOptions{})
	if err != nil {
		return nil, model.NewAppError("addLicense", "api.license.add_license.invalid_count.app_error", nil, err.Error(), http.StatusBadRequest)
	}

	if uniqueUserCount > int64(*license.Features.Users) {
		return nil, model.NewAppError("addLicense", "api.license.add_license.unique_users.app_error", map[string]interface{}{"Users": *license.Features.Users, "Count": uniqueUserCount}, "", http.StatusBadRequest)
	}

	if license.IsExpired() {
		return nil, model.NewAppError("addLicense", model.ExpiredLicenseError, nil, "", http.StatusBadRequest)
	}

	if *s.Config().JobSettings.RunJobs && s.Jobs != nil {
		if err := s.Jobs.StopWorkers(); err != nil && !errors.Is(err, jobs.ErrWorkersNotRunning) {
			mlog.Warn("Stopping job server workers failed", mlog.Err(err))
		}
	}

	if *s.Config().JobSettings.RunScheduler && s.Jobs != nil {
		if err := s.Jobs.StopSchedulers(); err != nil && !errors.Is(err, jobs.ErrSchedulersNotRunning) {
			mlog.Error("Stopping job server schedulers failed", mlog.Err(err))
		}
	}

	defer func() {
		// restart job server workers - this handles the edge case where a license file is uploaded, but the job server
		// doesn't start until the server is restarted, which prevents the 'run job now' buttons in system console from
		// functioning as expected
		if *s.Config().JobSettings.RunJobs && s.Jobs != nil {
			if err := s.Jobs.StartWorkers(); err != nil {
				mlog.Error("Starting job server workers failed", mlog.Err(err))
			}
		}
		if *s.Config().JobSettings.RunScheduler && s.Jobs != nil {
			if err := s.Jobs.StartSchedulers(); err != nil && !errors.Is(err, jobs.ErrSchedulersRunning) {
				mlog.Error("Starting job server schedulers failed", mlog.Err(err))
			}
		}
	}()

	if ok := s.SetLicense(&license); !ok {
		return nil, model.NewAppError("addLicense", model.ExpiredLicenseError, nil, "", http.StatusBadRequest)
	}

	record := &model.LicenseRecord{}
	record.Id = license.Id
	record.Bytes = string(licenseBytes)

	_, nErr := s.Store.License().Save(record)
	if nErr != nil {
		s.RemoveLicense()
		var appErr *model.AppError
		switch {
		case errors.As(nErr, &appErr):
			return nil, appErr
		default:
			return nil, model.NewAppError("addLicense", "api.license.add_license.save.app_error", nil, nErr.Error(), http.StatusInternalServerError)
		}
	}

	sysVar := &model.System{}
	sysVar.Name = model.SystemActiveLicenseId
	sysVar.Value = license.Id
	if err := s.Store.System().SaveOrUpdate(sysVar); err != nil {
		s.RemoveLicense()
		return nil, model.NewAppError("addLicense", "api.license.add_license.save_active.app_error", nil, "", http.StatusInternalServerError)
	}

	s.ReloadConfig()
	s.InvalidateAllCaches()

	return &license, nil
}

func (s *Server) SetLicense(license *model.License) bool {
	oldLicense := s.licenseValue.Load()

	defer func() {
		for _, listener := range s.licenseListeners {
			if oldLicense == nil {
				listener(nil, license)
			} else {
				listener(oldLicense.(*model.License), license)
			}
		}
	}()

	if license != nil {
		license.Features.SetDefaults()

		s.licenseValue.Store(license)
		s.clientLicenseValue.Store(utils.GetClientLicense(license))
		return true
	}

	s.licenseValue.Store((*model.License)(nil))
	s.clientLicenseValue.Store(map[string]string(nil))
	return false
}

func (s *Server) ValidateAndSetLicenseBytes(b []byte) bool {
	if success, licenseStr := utils.LicenseValidator.ValidateLicense(b); success {
		var license model.License
		if jsonErr := json.Unmarshal([]byte(licenseStr), &license); jsonErr != nil {
			mlog.Warn("Failed to decode license from JSON", mlog.Err(jsonErr))
			return false
		}
		s.SetLicense(&license)
		return true
	}

	mlog.Warn("No valid enterprise license found")
	return false
}

func (s *Server) SetClientLicense(m map[string]string) {
	s.clientLicenseValue.Store(m)
}

func (s *Server) ClientLicense() map[string]string {
	if clientLicense, _ := s.clientLicenseValue.Load().(map[string]string); clientLicense != nil {
		return clientLicense
	}
	return map[string]string{"IsLicensed": "false"}
}

func (s *Server) RemoveLicense() *model.AppError {
	if license, _ := s.licenseValue.Load().(*model.License); license == nil {
		return nil
	}

	mlog.Info("Remove license.", mlog.String("id", model.SystemActiveLicenseId))

	sysVar := &model.System{}
	sysVar.Name = model.SystemActiveLicenseId
	sysVar.Value = ""

	if err := s.Store.System().SaveOrUpdate(sysVar); err != nil {
		return model.NewAppError("RemoveLicense", "app.system.save.app_error", nil, err.Error(), http.StatusInternalServerError)
	}

	s.SetLicense(nil)
	s.ReloadConfig()
	s.InvalidateAllCaches()

	return nil
}

func (s *Server) AddLicenseListener(listener func(oldLicense, newLicense *model.License)) string {
	id := model.NewId()
	s.licenseListeners[id] = listener
	return id
}

func (s *Server) RemoveLicenseListener(id string) {
	delete(s.licenseListeners, id)
}

func (s *Server) GetSanitizedClientLicense() map[string]string {
	return utils.GetSanitizedClientLicense(s.ClientLicense())
}

// RequestTrialLicense request a trial license from the matterfoss official license server
func (s *Server) RequestTrialLicense(trialRequest *model.TrialLicenseRequest) *model.AppError {
	trialRequestJSON, jsonErr := json.Marshal(trialRequest)
	if jsonErr != nil {
		return model.NewAppError("RequestTrialLicense", "api.unmarshal_error", nil, jsonErr.Error(), http.StatusInternalServerError)
	}

	resp, err := http.Post(RequestTrialURL, "application/json", bytes.NewBuffer(trialRequestJSON))
	if err != nil {
		return model.NewAppError("RequestTrialLicense", "api.license.request_trial_license.app_error", nil, err.Error(), http.StatusBadRequest)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return model.NewAppError("RequestTrialLicense", "api.license.request_trial_license.app_error", nil,
			fmt.Sprintf("Unexpected HTTP status code %q returned by server", resp.Status), http.StatusInternalServerError)
	}

	licenseResponse := model.MapFromJSON(resp.Body)

	if _, ok := licenseResponse["license"]; !ok {
		return model.NewAppError("RequestTrialLicense", "api.license.request_trial_license.app_error", nil, licenseResponse["message"], http.StatusBadRequest)
	}

	if _, err := s.SaveLicense([]byte(licenseResponse["license"])); err != nil {
		return err
	}

	s.ReloadConfig()
	s.InvalidateAllCaches()

	return nil
}

// GenerateRenewalToken returns the current active token or generate a new one if
// the current active one has expired
func (s *Server) GenerateRenewalToken(expiration time.Duration) (string, *model.AppError) {
	license := s.License()
	if license == nil {
		// Clean renewal token if there is no license present
		if _, err := s.Store.System().PermanentDeleteByName(model.SystemLicenseRenewalToken); err != nil {
			mlog.Warn("error removing the renewal token", mlog.Err(err))
		}
		return "", model.NewAppError("GenerateRenewalToken", "app.license.generate_renewal_token.no_license", nil, "", http.StatusBadRequest)
	}

	if *license.Features.Cloud {
		return "", model.NewAppError("GenerateRenewalToken", "app.license.generate_renewal_token.bad_license", nil, "", http.StatusBadRequest)
	}

	currentToken, _ := s.Store.System().GetByName(model.SystemLicenseRenewalToken)
	if currentToken != nil {
		tokenIsValid, _ := s.renewalTokenValid(currentToken.Value, license.Customer.Email)
		if currentToken.Value != "" && tokenIsValid {
			return currentToken.Value, nil
		}
	}

	activeUsers, err := s.Store.User().Count(model.UserCountOptions{})
	if err != nil {
		return "", model.NewAppError("GenerateRenewalToken", "app.license.generate_renewal_token.app_error",
			nil, err.Error(), http.StatusInternalServerError)
	}

	expirationTime := time.Now().UTC().Add(expiration)
	claims := &JWTClaims{
		LicenseID:   license.Id,
		ActiveUsers: activeUsers,
		StandardClaims: jwt.StandardClaims{
			ExpiresAt: expirationTime.Unix(),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenString, err := token.SignedString([]byte(license.Customer.Email))
	if err != nil {
		return "", model.NewAppError("GenerateRenewalToken", "app.license.generate_renewal_token.app_error", nil, err.Error(), http.StatusInternalServerError)
	}
	err = s.Store.System().SaveOrUpdate(&model.System{
		Name:  model.SystemLicenseRenewalToken,
		Value: tokenString,
	})
	if err != nil {
		return "", model.NewAppError("GenerateRenewalToken", "app.license.generate_renewal_token.app_error", nil, err.Error(), http.StatusInternalServerError)
	}
	return tokenString, nil
}

func (s *Server) renewalTokenValid(tokenString, signingKey string) (bool, error) {
	claims := &JWTClaims{}

	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(signingKey), nil
	})
	if err != nil {
		return false, errors.Wrapf(err, "Error validating JWT token")
	}
	if !token.Valid {
		return false, errors.New("invalid JWT token")
	}
	expirationTime := time.Unix(claims.ExpiresAt, 0)
	if expirationTime.Before(time.Now().UTC()) {
		return false, nil
	}
	return true, nil
}

// GenerateLicenseRenewalLink returns a link that points to the CWS where clients can renew license
func (s *Server) GenerateLicenseRenewalLink() (string, string, *model.AppError) {
	renewalToken, err := s.GenerateRenewalToken(JWTDefaultTokenExpiration)
	if err != nil {
		return "", "", err
	}
	renewalLink := LicenseRenewalURL + "?token=" + renewalToken
	return renewalLink, renewalToken, nil
}
