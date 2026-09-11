#!/usr/bin/env python3
"""Grant personal Drive edits through Google's local desktop OAuth flow."""
import base64
import hashlib
import http.server
import json
import os
import secrets
import shutil
import webbrowser
import sys
import time
import urllib.parse

import requests

from settings import CONFIG_DIR as BASE, expected_identity
SCOPE = 'https://www.googleapis.com/auth/drive'
EXPECTED_EMAIL, EXPECTED_PROJECT = expected_identity()


def private_json(path, value):
    temp = path.with_name(path.name + '.tmp-' + secrets.token_hex(4))
    fd = os.open(temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(fd, 'w') as handle:
            json.dump(value, handle, indent=2)
            handle.write('\n')
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, path)
    finally:
        temp.unlink(missing_ok=True)


def main():
    key = json.loads((BASE / 'gcp-oauth.keys.json').read_text())['installed']
    if key.get('project_id') != EXPECTED_PROJECT:
        raise RuntimeError('OAuth client is not the expected personal project')
    state = secrets.token_urlsafe(32)
    verifier = secrets.token_urlsafe(64)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b'=').decode()
    response = {}

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            parsed = urllib.parse.urlparse(self.path)
            query = urllib.parse.parse_qs(parsed.query)
            if parsed.path != '/oauth/callback':
                self.send_error(404)
                return
            received_state = query.get('state', [''])[0]
            if not secrets.compare_digest(received_state, state):
                self.send_error(400, 'Invalid OAuth state')
                return
            response.update({name: values[0] for name, values in query.items()})
            self.send_response(200)
            self.send_header('Content-Type', 'text/html; charset=utf-8')
            self.send_header('Cache-Control', 'no-store')
            self.send_header('Referrer-Policy', 'no-referrer')
            self.end_headers()
            self.wfile.write(b'<html><body style="font:18px system-ui;padding:48px"><h1>Google sign-in received.</h1><p>Return to the terminal. The account and access are being checked.</p></body></html>')

        def log_message(self, *_args):
            pass

    with http.server.HTTPServer(('127.0.0.1', 0), Handler) as server:
        server.timeout = 1
        redirect = f'http://127.0.0.1:{server.server_port}/oauth/callback'
        auth_url = 'https://accounts.google.com/o/oauth2/v2/auth?' + urllib.parse.urlencode({
            'client_id': key['client_id'], 'redirect_uri': redirect,
            'response_type': 'code', 'scope': SCOPE, 'access_type': 'offline',
            'prompt': 'select_account consent', 'login_hint': EXPECTED_EMAIL,
            'state': state, 'code_challenge': challenge, 'code_challenge_method': 'S256',
        })
        print('AUTH_URL: ' + auth_url, flush=True)
        print('WAITING: choose ' + EXPECTED_EMAIL + ' and approve Drive access.', flush=True)
        webbrowser.open(auth_url)
        deadline = time.monotonic() + 1200
        while not response and time.monotonic() < deadline:
            server.handle_request()
    if 'code' not in response:
        raise RuntimeError('Google consent was not completed. Existing credentials remain unchanged.')

    exchange = requests.post('https://oauth2.googleapis.com/token', data={
        'code': response['code'], 'client_id': key['client_id'],
        'client_secret': key['client_secret'], 'redirect_uri': redirect,
        'grant_type': 'authorization_code', 'code_verifier': verifier,
    }, timeout=30)
    if exchange.status_code != 200:
        raise RuntimeError(f'Google token exchange returned HTTP {exchange.status_code}')
    token = exchange.json()
    if SCOPE not in token.get('scope', '').split() or not token.get('refresh_token'):
        raise RuntimeError('Google did not grant full Drive scope and a refresh token. Existing credentials remain unchanged.')

    renewed = requests.post('https://oauth2.googleapis.com/token', data={
        'client_id': key['client_id'], 'client_secret': key['client_secret'],
        'refresh_token': token['refresh_token'], 'grant_type': 'refresh_token',
    }, timeout=30)
    if renewed.status_code != 200:
        raise RuntimeError(f'Google token renewal returned HTTP {renewed.status_code}')
    refreshed = renewed.json()
    if SCOPE not in refreshed.get('scope', token['scope']).split():
        raise RuntimeError('Renewed token lacks Drive edit permission')
    identity = requests.get('https://www.googleapis.com/drive/v3/about',
        params={'fields': 'user(emailAddress)'},
        headers={'Authorization': 'Bearer ' + refreshed['access_token']}, timeout=30)
    if identity.status_code != 200:
        raise RuntimeError(f'Google account check returned HTTP {identity.status_code}')
    email = identity.json()['user']['emailAddress']
    if email != EXPECTED_EMAIL:
        raise RuntimeError('Signed-in account did not match the configured email. Existing credentials remain unchanged.')

    credential = {
        'access_token': refreshed['access_token'], 'refresh_token': token['refresh_token'],
        'scope': refreshed.get('scope', token['scope']),
        'token_type': refreshed.get('token_type', 'Bearer'),
        'expiry_date': int(time.time() * 1000) + int(refreshed.get('expires_in', 3600)) * 1000,
    }
    if 'refresh_token_expires_in' in token:
        credential['refresh_token_expires_in'] = token['refresh_token_expires_in']
    backup_dir = BASE / 'backups' / (time.strftime('%Y-%m-%d-%H%M%S') + '-personal-edit')
    backup_dir.mkdir(parents=True, mode=0o700)
    current = BASE / 'credentials.json'
    if current.exists():
        shutil.copy2(current, backup_dir / 'credentials.json')
        os.chmod(backup_dir / 'credentials.json', 0o600)
    private_json(current, credential)
    proof = {'email': email, 'project': key['project_id'], 'scope': credential['scope'],
             'refresh_test': 'passed', 'checked_at': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime()),
             'refresh_token_expires_in': token.get('refresh_token_expires_in')}
    private_json(BASE / 'edit-auth-verification.json', proof)
    print('SUCCESS: ' + json.dumps(proof), flush=True)


if __name__ == '__main__':
    try:
        main()
    except Exception as error:
        print('FAILED: ' + str(error), file=sys.stderr, flush=True)
        raise SystemExit(1)
