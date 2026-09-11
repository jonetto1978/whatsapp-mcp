"""Configure the expected account and an existing Desktop OAuth client file."""
import argparse
import json
import os
from pathlib import Path

from settings import CONFIG_DIR


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--email', required=True)
    parser.add_argument('--project', required=True)
    parser.add_argument('--client-file', type=Path, required=True)
    args = parser.parse_args()
    client = json.loads(args.client_file.read_text())
    if client.get('installed', {}).get('project_id') != args.project:
        parser.error('Use a Desktop OAuth client from the requested project')
    CONFIG_DIR.mkdir(parents=True, exist_ok=True)
    values = {'settings.json': {'expected_email': args.email, 'expected_project': args.project},
              'gcp-oauth.keys.json': client}
    for name, value in values.items():
        path = CONFIG_DIR / name
        if path.exists() and json.loads(path.read_text()) != value:
            raise SystemExit('Existing configuration differs: ' + str(path) + '. Review it before replacing it.')
    for name, value in values.items():
        path = CONFIG_DIR / name
        if not path.exists():
            with os.fdopen(os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), 'w') as handle:
                json.dump(value, handle, indent=2)
                handle.write('\n')
    print('Drive account configured. Run authorize-edit.py to sign in. Credentials stay outside the repository.')


if __name__ == '__main__':
    main()
