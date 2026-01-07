package oidc

import (
	"html/template"
	"io"
)

// Templates holds the parsed HTML templates for the OIDC provider.
type Templates struct {
	login   *template.Template
	consent *template.Template
}

// NewTemplates creates and parses the OIDC templates.
func NewTemplates() (*Templates, error) {
	login, err := template.New("login").Parse(loginHTML)
	if err != nil {
		return nil, err
	}
	consent, err := template.New("consent").Parse(consentHTML)
	if err != nil {
		return nil, err
	}
	return &Templates{
		login:   login,
		consent: consent,
	}, nil
}

// ExecuteLogin renders the login page.
func (t *Templates) ExecuteLogin(w io.Writer, data any) error {
	return t.login.Execute(w, data)
}

// ExecuteConsent renders the consent page.
func (t *Templates) ExecuteConsent(w io.Writer, data any) error {
	return t.consent.Execute(w, data)
}

const loginHTML = `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Piccolo Login</title>
    <style>
        body { font-family: sans-serif; display: flex; justify-content: center; align-items: center; height: 100vh; background-color: #f0f2f5; margin: 0; }
        .card { background: white; padding: 2rem; border-radius: 8px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); width: 100%; max-width: 400px; }
        h1 { margin-top: 0; color: #333; text-align: center; }
        .form-group { margin-bottom: 1rem; }
        label { display: block; margin-bottom: 0.5rem; color: #666; }
        input { width: 100%; padding: 0.5rem; border: 1px solid #ddd; border-radius: 4px; box-sizing: border-box; }
        button { width: 100%; padding: 0.75rem; background-color: #007bff; color: white; border: none; border-radius: 4px; cursor: pointer; font-size: 1rem; }
        button:hover { background-color: #0056b3; }
        .error { color: #dc3545; margin-bottom: 1rem; text-align: center; }
    </style>
</head>
<body>
    <div class="card">
        <h1>Sign In</h1>
        {{if .Error}}
            <div class="error">{{.Error}}</div>
        {{end}}
        <form method="POST" action="{{.PostURL}}">
            <div class="form-group">
                <label for="username">Username</label>
                <input type="text" id="username" name="username" required autofocus>
            </div>
            <div class="form-group">
                <label for="password">Password</label>
                <input type="password" id="password" name="password" required>
            </div>
            <button type="submit">Sign In</button>
        </form>
    </div>
</body>
</html>
`

const consentHTML = `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Piccolo Access Request</title>
    <style>
        body { font-family: sans-serif; display: flex; justify-content: center; align-items: center; height: 100vh; background-color: #f0f2f5; margin: 0; }
        .card { background: white; padding: 2rem; border-radius: 8px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); width: 100%; max-width: 400px; }
        h1 { margin-top: 0; color: #333; text-align: center; }
        p { color: #666; text-align: center; margin-bottom: 2rem; }
        .scopes { background: #f8f9fa; padding: 1rem; border-radius: 4px; margin-bottom: 2rem; }
        .scope { display: flex; align-items: center; margin-bottom: 0.5rem; }
        .scope:last-child { margin-bottom: 0; }
        .actions { display: flex; gap: 1rem; }
        button { flex: 1; padding: 0.75rem; border: none; border-radius: 4px; cursor: pointer; font-size: 1rem; }
        .btn-allow { background-color: #007bff; color: white; }
        .btn-allow:hover { background-color: #0056b3; }
        .btn-deny { background-color: #dc3545; color: white; }
        .btn-deny:hover { background-color: #a71d2a; }
    </style>
</head>
<body>
    <div class="card">
        <h1>Access Request</h1>
        <p><strong>{{.ClientName}}</strong> wants to access your Piccolo account.</p>
        
        <form method="POST" action="{{.PostURL}}">
            <div class="actions">
                <button type="submit" name="deny" value="true" class="btn-deny">Deny</button>
                <button type="submit" name="allow" value="true" class="btn-allow">Allow</button>
            </div>
        </form>
    </div>
</body>
</html>
`
