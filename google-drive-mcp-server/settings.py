"""Machine-local Drive configuration; no credentials belong in the repository."""
import json
import os
from pathlib import Path

CONFIG_DIR = Path(os.environ.get('WHATSAPP_DRIVE_CONFIG_DIR',
    str(Path.home() / '.config' / 'mcp' / 'personal' / 'google-drive'))).expanduser().resolve()


def expected_identity():
    path = CONFIG_DIR / 'settings.json'
    settings = json.loads(path.read_text()) if path.is_file() else {}
    return (os.environ.get('WHATSAPP_DRIVE_EXPECTED_EMAIL', settings.get('expected_email', '')),
            os.environ.get('WHATSAPP_DRIVE_EXPECTED_PROJECT', settings.get('expected_project', '')))
