// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package app

import (
	"errors"
	"fmt"
	"net/http"
	"os/user"

	"github.com/cjdelisle/matterfoss-server/v6/audit"
	"github.com/cjdelisle/matterfoss-server/v6/config"
	"github.com/cjdelisle/matterfoss-server/v6/model"
	"github.com/cjdelisle/matterfoss-server/v6/shared/mlog"
	"github.com/cjdelisle/matterfoss-server/v6/store"
)

const (
	RestLevelID        = 240
	RestContentLevelID = 241
	RestPermsLevelID   = 242
	CLILevelID         = 243
)

var (
	LevelAPI     = mlog.LvlAuditAPI
	LevelContent = mlog.LvlAuditContent
	LevelPerms   = mlog.LvlAuditPerms
	LevelCLI     = mlog.LvlAuditCLI
)

func (a *App) GetAudits(userID string, limit int) (model.Audits, *model.AppError) {
	audits, err := a.Srv().Store.Audit().Get(userID, 0, limit)
	if err != nil {
		var outErr *store.ErrOutOfBounds
		switch {
		case errors.As(err, &outErr):
			return nil, model.NewAppError("GetAudits", "app.audit.get.limit.app_error", nil, err.Error(), http.StatusBadRequest)
		default:
			return nil, model.NewAppError("GetAudits", "app.audit.get.finding.app_error", nil, err.Error(), http.StatusInternalServerError)
		}
	}
	return audits, nil
}

func (a *App) GetAuditsPage(userID string, page int, perPage int) (model.Audits, *model.AppError) {
	audits, err := a.Srv().Store.Audit().Get(userID, page*perPage, perPage)
	if err != nil {
		var outErr *store.ErrOutOfBounds
		switch {
		case errors.As(err, &outErr):
			return nil, model.NewAppError("GetAuditsPage", "app.audit.get.limit.app_error", nil, err.Error(), http.StatusBadRequest)
		default:
			return nil, model.NewAppError("GetAuditsPage", "app.audit.get.finding.app_error", nil, err.Error(), http.StatusInternalServerError)
		}
	}
	return audits, nil
}

// LogAuditRec logs an audit record using default LvlAuditCLI.
func (a *App) LogAuditRec(rec *audit.Record, err error) {
	a.LogAuditRecWithLevel(rec, mlog.LvlAuditCLI, err)
}

// LogAuditRecWithLevel logs an audit record using specified Level.
func (a *App) LogAuditRecWithLevel(rec *audit.Record, level mlog.Level, err error) {
	if rec == nil {
		return
	}
	if err != nil {
		if appErr, ok := err.(*model.AppError); ok {
			rec.AddMeta("err", appErr.Error())
			rec.AddMeta("code", appErr.StatusCode)
		} else {
			rec.AddMeta("err", err)
		}
		rec.Fail()
	}
	a.Srv().Audit.LogRecord(level, *rec)
}

// MakeAuditRecord creates a audit record pre-populated with defaults.
func (a *App) MakeAuditRecord(event string, initialStatus string) *audit.Record {
	var userID string
	user, err := user.Current()
	if err == nil {
		userID = fmt.Sprintf("%s:%s", user.Uid, user.Username)
	}

	rec := &audit.Record{
		APIPath:   "",
		Event:     event,
		Status:    initialStatus,
		UserID:    userID,
		SessionID: "",
		Client:    fmt.Sprintf("server %s-%s", model.BuildNumber, model.BuildHash),
		IPAddress: "",
		Meta:      audit.Meta{audit.KeyClusterID: a.GetClusterId()},
	}
	rec.AddMetaTypeConverter(model.AuditModelTypeConv)

	return rec
}

func (s *Server) configureAudit(adt *audit.Audit, bAllowAdvancedLogging bool) error {
	adt.OnQueueFull = s.onAuditTargetQueueFull
	adt.OnError = s.onAuditError

	var logConfigSrc config.LogConfigSrc
	dsn := *s.Config().ExperimentalAuditSettings.AdvancedLoggingConfig
	if bAllowAdvancedLogging && dsn != "" {
		var err error
		logConfigSrc, err = config.NewLogConfigSrc(dsn, s.configStore.Store)
		if err != nil {
			return fmt.Errorf("invalid config source for audit, %w", err)
		}
		mlog.Debug("Loaded audit configuration", mlog.String("source", dsn))
	}

	// ExperimentalAuditSettings provides basic file audit (E0, E10); logConfigSrc provides advanced config (E20).
	cfg, err := config.MloggerConfigFromAuditConfig(s.Config().ExperimentalAuditSettings, logConfigSrc)
	if err != nil {
		return fmt.Errorf("invalid config for audit, %w", err)
	}

	return adt.Configure(cfg)
}

func (s *Server) onAuditTargetQueueFull(qname string, maxQSize int) bool {
	mlog.Error("Audit queue full, dropping record.", mlog.String("qname", qname), mlog.Int("queueSize", maxQSize))
	return true // drop it
}

func (s *Server) onAuditError(err error) {
	mlog.Error("Audit Error", mlog.Err(err))
}
