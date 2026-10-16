package httpapi

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"markemind/dials/internal/auth"
	"markemind/dials/internal/emailcheck"
	"markemind/dials/internal/importer"
	"markemind/dials/internal/mail"
	"markemind/dials/internal/platform"
	"markemind/dials/internal/search"
	"markemind/dials/internal/unlock"
)

var emailReasons = map[string]string{
	"disposable":    "Temporary or disposable inboxes can't be used. Please enter the email you actually read.",
	"free_provider": "Please use your work email. Free mailbox providers are switched off right now.",
	"null_mx":       "That domain doesn't accept email. Check the address for a typo.",
	"no_mx":         "That domain can't receive email. Check the address for a typo.",
	"undeliverable": "That mailbox doesn't seem to exist. Check the address for a typo.",
}

func emailMessage(reason string) string {
	if m, ok := emailReasons[reason]; ok {
		return m
	}
	return "That email address doesn't look right. Check it and try again."
}

func (a *App) writeGuard(w http.ResponseWriter) bool {
	if err := a.Guard.AllowWrite(); err != nil {
		w.Header().Set("Retry-After", "300")
		writeErr(w, http.StatusServiceUnavailable, "read_only", "Unlocking is paused for a few minutes of maintenance. Browsing and counts still work.")
		return false
	}
	return true
}

func (a *App) limited(w http.ResponseWriter, l *auth.Limiter, key, msg string) bool {
	if ok, wait := l.Take(key); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(int(wait.Seconds())+1))
		writeErrExtra(w, http.StatusTooManyRequests, "rate_limited", msg, map[string]int{"retry_after": int(wait.Seconds()) + 1})
		return true
	}
	return false
}

func (a *App) sendOTPError(w http.ResponseWriter, ctx context.Context, err error) {
	switch {
	case errors.Is(err, auth.ErrTooManySends):
		writeErr(w, http.StatusTooManyRequests, "otp_rate", "You've requested three codes in the last hour. Use the latest code or try again later.")
	case errors.Is(err, mail.ErrQuota):
		a.Alerter.Alert(ctx, "mail_quota", "Email quota exhausted", "Sign-in codes could not be sent: every provider hit its daily cap.")
		writeErr(w, http.StatusServiceUnavailable, "email_unavailable", "We can't send sign-in emails right now. Please try again in a few hours.")
	case mail.IsPermanent(err):
		writeErr(w, http.StatusUnprocessableEntity, "invalid_email", "We couldn't deliver to that address. Check it for a typo.")
	default:
		a.Log.Error("otp send failed", "err", err)
		a.Alerter.Alert(ctx, "mail_fail", "Sign-in email failed", err.Error())
		writeErr(w, http.StatusBadGateway, "email_failed", "We couldn't send the code just now. Please try again.")
	}
}

// ---------- sign-in ----------

type authStartIn struct {
	Email     string `json:"email"`
	Turnstile string `json:"turnstile"`
}

func (a *App) apiAuthStart(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.sameOrigin(w, r) || !a.writeGuard(w) {
		return
	}
	var in authStartIn
	if !decodeJSON(w, r, &in, 8<<10) {
		return
	}
	ctx, ip := r.Context(), clientIP(r)
	if a.limited(w, a.limits.authStart, ip, "Too many sign-in attempts from your network. Please wait a little.") {
		return
	}
	if err := a.Turnstile.Verify(ctx, in.Turnstile, ip, ""); err != nil {
		writeErr(w, http.StatusForbidden, "bot_check", "The anti-bot check failed. Please complete it again.")
		return
	}
	email, v := a.Checker.Check(ctx, in.Email)
	if !v.OK {
		reason := v.Reason
		if strings.HasPrefix(reason, "invalid_syntax") {
			reason = "invalid_syntax"
		}
		writeErrExtra(w, http.StatusUnprocessableEntity, "invalid_email", emailMessage(reason), map[string]string{"reason": reason})
		return
	}
	if !a.Settings.Get().Limits.SignupsEnabled {
		if _, err := a.Users.ByEmail(ctx, email); err != nil {
			writeErr(w, http.StatusForbidden, "signups_closed", "New sign-ups are paused right now. Please check back soon.")
			return
		}
	}
	if err := a.OTP.Start(ctx, email, "login"); err != nil {
		a.sendOTPError(w, ctx, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": email})
}

type authVerifyIn struct {
	Email     string `json:"email"`
	Code      string `json:"code"`
	Subscribe *bool  `json:"subscribe"`
}

func (a *App) otpError(w http.ResponseWriter, err error) {
	var ice *auth.InvalidCodeError
	switch {
	case errors.As(err, &ice):
		writeErrExtra(w, http.StatusBadRequest, "otp_invalid", fmt.Sprintf("That code isn't right. %d attempts left.", ice.Remaining), map[string]int{"remaining": ice.Remaining})
	case errors.Is(err, auth.ErrLocked):
		writeErr(w, http.StatusTooManyRequests, "otp_locked", "Too many wrong codes. Request a new code to continue.")
	case errors.Is(err, auth.ErrExpired):
		writeErr(w, http.StatusGone, "otp_expired", "That code has expired. Request a new one.")
	case errors.Is(err, auth.ErrNoCode):
		writeErr(w, http.StatusBadRequest, "otp_none", "Request a code first.")
	default:
		a.Log.Error("otp verify", "err", err)
		writeErr(w, http.StatusInternalServerError, "server_error", "Something went wrong. Please retry.")
	}
}

func (a *App) apiAuthVerify(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.sameOrigin(w, r) || !a.writeGuard(w) {
		return
	}
	var in authVerifyIn
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	ctx := r.Context()
	if a.limited(w, a.limits.authVerify, clientIP(r), "Too many attempts from your network. Please wait a little.") {
		return
	}
	email, ok, _ := emailcheck.Syntax(in.Email)
	if !ok {
		writeErr(w, http.StatusBadRequest, "otp_none", "Request a code first.")
		return
	}
	if err := a.OTP.Verify(ctx, email, "login", in.Code); err != nil {
		a.otpError(w, err)
		return
	}
	sub := a.Settings.Get().Site.SubscribeDefault
	if in.Subscribe != nil {
		sub = *in.Subscribe
	}
	u, err := a.Users.Upsert(ctx, email, sub)
	if errors.Is(err, auth.ErrBlocked) {
		writeErr(w, http.StatusForbidden, "blocked", "This account has been suspended. Contact us if you think this is a mistake.")
		return
	}
	if err != nil {
		a.queryError(w, err)
		return
	}
	tok, exp, err := a.Sessions.Create(ctx, u.ID)
	if err != nil {
		a.queryError(w, err)
		return
	}
	a.setSession(w, tok, exp)
	writeJSON(w, http.StatusOK, map[string]any{"user": map[string]any{"email": u.Email, "subscribed": u.Subscribed}})
}

func (a *App) apiLogout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.sameOrigin(w, r) {
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.Sessions.Delete(r.Context(), c.Value)
	}
	a.clearSession(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) apiMe(w http.ResponseWriter, r *http.Request) {
	uid, ok := a.user(w, r)
	if !ok {
		return
	}
	u, err := a.Users.Get(r.Context(), uid)
	if err != nil {
		a.clearSession(w)
		writeErr(w, http.StatusUnauthorized, "unauthorized", "Please sign in.")
		return
	}
	sm, _ := a.Unlock.Summary(r.Context(), uid)
	writeJSON(w, http.StatusOK, map[string]any{"email": u.Email, "subscribed": u.Subscribed, "ads_today": sm.AdsToday,
		"max_ads": sm.MaxAds, "unlocked_total": sm.Unlocked})
}

// ---------- unlock ----------

func (a *App) unlockError(w http.ResponseWriter, err error) {
	var ts *unlock.TooSoonError
	switch {
	case errors.As(err, &ts):
		writeErrExtra(w, http.StatusTooEarly, "too_early", "Almost there. The sponsor break isn't finished yet.", map[string]int64{"wait_ms": ts.Wait.Milliseconds()})
	case errors.Is(err, unlock.ErrTokenUsed):
		writeErr(w, http.StatusConflict, "token_used", "That sponsor break was already used. Start a new one.")
	case errors.Is(err, unlock.ErrTokenInvalid):
		writeErr(w, http.StatusConflict, "token_invalid", "That sponsor break expired. Start a new one.")
	case errors.Is(err, unlock.ErrDailyLimit), errors.Is(err, unlock.ErrIPLimit):
		writeErr(w, http.StatusTooManyRequests, "limit_reached", "You've reached today's unlock limit. It resets at midnight UTC. Need more? Ask us for a bulk list.")
	case errors.Is(err, unlock.ErrNothingLeft):
		writeErr(w, http.StatusConflict, "nothing_left", "You've unlocked every contact for these filters.")
	default:
		a.queryError(w, err)
	}
}

func (a *App) apiView(w http.ResponseWriter, r *http.Request) {
	uid, ok := a.user(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f, ok := a.parseFilter(w, q)
	if !ok {
		return
	}
	after := q.Get("after")
	if len(after) > 80 {
		after = ""
	}
	v, err := a.Unlock.View(r.Context(), uid, f, q.Get("newest") == "1", after)
	if err != nil {
		a.queryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *App) apiExport(w http.ResponseWriter, r *http.Request) {
	uid, ok := a.user(w, r)
	if !ok {
		return
	}
	if a.limited(w, a.limits.export, fmt.Sprint(uid), "You've exported a lot this hour. Please try again later.") {
		return
	}
	f, ok := a.parseFilter(w, r.URL.Query())
	if !ok {
		return
	}
	select {
	case a.exportSem <- struct{}{}:
		defer func() { <-a.exportSem }()
	default:
		w.Header().Set("Retry-After", "10")
		writeErr(w, http.StatusServiceUnavailable, "busy", "Exports are busy right now. Please try again in a few seconds.")
		return
	}
	// stream straight to the client: memory stays flat however many rows are exported
	name := "free-direct-dials-" + a.Clock.Now().UTC().Format("20060102") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Write([]byte("\xef\xbb\xbf")) // BOM so Excel opens UTF-8 correctly
	if _, err := a.Unlock.Export(r.Context(), uid, f, w); err != nil {
		a.Log.Warn("export interrupted", "err", err) // headers are sent; the client sees a truncated file
	}
}

func (a *App) apiAdStart(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.sameOrigin(w, r) {
		return
	}
	uid, ok := a.user(w, r)
	if !ok || !a.writeGuard(w) {
		return
	}
	t, err := a.Unlock.Start(r.Context(), uid, clientIP(r))
	if err != nil {
		a.unlockError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

type claimIn struct {
	Token  string `json:"token"`
	Filter string `json:"filter"` // canonical query string
}

func (a *App) apiAdClaim(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.sameOrigin(w, r) {
		return
	}
	uid, ok := a.user(w, r)
	if !ok || !a.writeGuard(w) {
		return
	}
	var in claimIn
	if !decodeJSON(w, r, &in, 8<<10) {
		return
	}
	q, err := url.ParseQuery(in.Filter)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_filter", "Invalid filter.")
		return
	}
	f, ok := a.parseFilter(w, q)
	if !ok {
		return
	}
	res, err := a.Unlock.Claim(r.Context(), uid, in.Token, f, clientIP(r))
	if err != nil {
		a.unlockError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- unsubscribe (RFC 8058 one-click + confirmation page) ----------

func (a *App) apiUnsub(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, 4<<10))
	tok := r.URL.Query().Get("t")
	email, err := a.Users.Unsubscribe(r.Context(), tok)
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		d := a.baseData("unsubscribe")
		d.Title, d.NoIndex, d.Done = "Unsubscribed | Free Direct Dials", true, true
		if err != nil {
			d.Response = "This link isn't valid any more. You may already be unsubscribed."
		} else {
			d.Response = "Done. " + search.MaskEmail(email) + " won't get new-data emails any more."
		}
		var buf bytes.Buffer
		a.Tpl.Render(&buf, "unsubscribe", d)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", a.publicCSP())
		w.Write(buf.Bytes())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": err == nil})
}

// ---------- data removal ----------

type removeStartIn struct {
	Email     string `json:"email"`
	Turnstile string `json:"turnstile"`
}

func (a *App) apiRemoveStart(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.sameOrigin(w, r) {
		return
	}
	var in removeStartIn
	if !decodeJSON(w, r, &in, 8<<10) {
		return
	}
	ctx, ip := r.Context(), clientIP(r)
	if a.limited(w, a.limits.removal, ip, "Too many removal requests from your network. Please wait an hour.") {
		return
	}
	if err := a.Turnstile.Verify(ctx, in.Turnstile, ip, ""); err != nil {
		writeErr(w, http.StatusForbidden, "bot_check", "The anti-bot check failed. Please complete it again.")
		return
	}
	email, ok, _ := emailcheck.Syntax(in.Email)
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "invalid_email", emailMessage("invalid_syntax"))
		return
	}
	if err := a.OTP.Start(ctx, email, "remove"); err != nil {
		a.sendOTPError(w, ctx, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": email})
}

func (a *App) apiRemoveVerify(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.sameOrigin(w, r) {
		return
	}
	var in authVerifyIn
	if !decodeJSON(w, r, &in, 4<<10) {
		return
	}
	ctx := r.Context()
	if a.limited(w, a.limits.authVerify, clientIP(r), "Too many attempts from your network. Please wait a little.") {
		return
	}
	email, ok, _ := emailcheck.Syntax(in.Email)
	if !ok {
		writeErr(w, http.StatusBadRequest, "otp_none", "Request a code first.")
		return
	}
	if err := a.OTP.Verify(ctx, email, "remove", in.Code); err != nil {
		a.otpError(w, err)
		return
	}
	now := a.Clock.Now().Unix()
	// block future uploads first, even if the person isn't in the data today
	if err := importer.Suppress(ctx, a.DB, "email", email, now); err != nil {
		a.queryError(w, err)
		return
	}
	n, err := a.Store.Remove(ctx, "email", email, now)
	status, note := "done", fmt.Sprintf("self-service, verified by code; %d record(s) deleted", n)
	if err != nil {
		// a database was unreachable: the identity is suppressed and the rest is
		// queued for the admin (Removals), inside the 30-day deadline
		a.Log.Warn("self-service removal incomplete", "err", err)
		status, note = "pending", note+"; incomplete, a database was unreachable: process again ("+err.Error()+")"
	}
	value := search.MaskEmail(email)
	if status == "pending" {
		value = email // the admin needs the real address to finish the job
	}
	a.DB.W.ExecContext(ctx, `INSERT INTO removals(kind, value, note, status, ts, done_ts) VALUES('email', ?, ?, ?, ?, ?)`,
		value, note, status, now, now)
	if n > 0 {
		a.purgeData(context.WithoutCancel(ctx))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n, "pending": status == "pending"})
}

type removeRequestIn struct {
	Kind      string `json:"kind"`
	Value     string `json:"value"`
	Contact   string `json:"contact"`
	Note      string `json:"note"`
	Turnstile string `json:"turnstile"`
}

func (a *App) apiRemoveRequest(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !a.sameOrigin(w, r) {
		return
	}
	var in removeRequestIn
	if !decodeJSON(w, r, &in, 8<<10) {
		return
	}
	ctx, ip := r.Context(), clientIP(r)
	if a.limited(w, a.limits.removal, ip, "Too many removal requests from your network. Please wait an hour.") {
		return
	}
	if err := a.Turnstile.Verify(ctx, in.Turnstile, ip, ""); err != nil {
		writeErr(w, http.StatusForbidden, "bot_check", "The anti-bot check failed. Please complete it again.")
		return
	}
	if in.Kind != "linkedin" && in.Kind != "phone" && in.Kind != "other" {
		writeErr(w, http.StatusBadRequest, "bad_request", "Choose what identifies your record.")
		return
	}
	value := strings.TrimSpace(in.Value)
	if len(value) < 3 || len(value) > 300 || len(in.Note) > 1000 || len(in.Contact) > 254 {
		writeErr(w, http.StatusBadRequest, "bad_request", "Please check the details you entered.")
		return
	}
	note := strings.TrimSpace(in.Note)
	if c := strings.TrimSpace(in.Contact); c != "" {
		if e, ok, _ := emailcheck.Syntax(c); ok {
			note = "reply-to: " + e + "\n" + note
		}
	}
	now := a.Clock.Now().Unix()
	if _, err := a.DB.W.ExecContext(ctx, `INSERT INTO removals(kind, value, note, status, ts) VALUES(?,?,?,'pending',?)`, in.Kind, value, note, now); err != nil {
		a.queryError(w, err)
		return
	}
	a.Alerter.Alert(context.WithoutCancel(ctx), "removal_"+platform.SHA256Hex(value)[:8], "New data removal request",
		"A "+in.Kind+" removal request is waiting in Superadmin → Removals (30-day deadline).")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------- email provider webhooks (bounces / complaints) ----------

func (a *App) apiMailHook(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	tok := a.Cfg.MailWebhookToken
	if tok == "" || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("k")), []byte(tok)) != 1 {
		writeErr(w, http.StatusNotFound, "not_found", "Not found.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Bad body.")
		return
	}
	type ev struct{ email, kind string }
	var events []ev
	switch r.PathValue("provider") {
	case "brevo":
		var one struct {
			Event string `json:"event"`
			Email string `json:"email"`
		}
		var many []struct {
			Event string `json:"event"`
			Email string `json:"email"`
		}
		if json.Unmarshal(body, &many) != nil {
			json.Unmarshal(body, &one)
			many = append(many, one)
		}
		for _, e := range many {
			events = append(events, ev{e.Email, e.Event})
		}
	case "resend":
		var e struct {
			Type string `json:"type"`
			Data struct {
				To []string `json:"to"`
			} `json:"data"`
		}
		json.Unmarshal(body, &e)
		for _, to := range e.Data.To {
			events = append(events, ev{to, e.Type})
		}
	default:
		writeErr(w, http.StatusNotFound, "not_found", "Unknown provider.")
		return
	}
	for _, e := range events {
		email, ok, _ := emailcheck.Syntax(e.email)
		if !ok {
			continue
		}
		switch e.kind {
		case "hard_bounce", "blocked", "invalid_email", "spam", "complaint", "email.bounced", "email.complained":
			a.DB.W.ExecContext(r.Context(), `UPDATE users SET bounced=1 WHERE email=?`, email)
		case "unsubscribed", "unsubscribe":
			a.DB.W.ExecContext(r.Context(), `UPDATE users SET subscribed=0 WHERE email=?`, email)
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"processed": len(events)})
}

// ---------- Cloudflare cache purge (optional) ----------

func (a *App) purgeCDN(ctx context.Context, paths ...string) {
	if a.Cfg.CFAPIToken == "" || a.Cfg.CFZoneID == "" {
		return
	}
	files := make([]string, 0, len(paths))
	for _, p := range paths {
		files = append(files, a.Cfg.URL(p))
	}
	go func() {
		a.purgeMu.Lock()
		defer a.purgeMu.Unlock()
		body, _ := json.Marshal(map[string]any{"files": files})
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(cctx, http.MethodPost,
			"https://api.cloudflare.com/client/v4/zones/"+url.PathEscape(a.Cfg.CFZoneID)+"/purge_cache", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+a.Cfg.CFAPIToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			a.Log.Warn("cloudflare purge failed", "err", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			a.Log.Warn("cloudflare purge failed", "status", resp.StatusCode)
		}
	}()
}
