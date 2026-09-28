"""Bounded native Sheets reads and content-only edits with read-back checks."""

from copy import deepcopy
import hashlib
import json
import math
import re

from googleapiclient.errors import HttpError


MAX_CELLS = 2000
MAX_TEXT = 50000
CELL_FIELDS = ('userEnteredValue,formattedValue,userEnteredFormat,'
               'dataValidation,note,textFormatRuns,chipRuns')
STORED_FIELDS = ('userEnteredValue', 'userEnteredFormat', 'dataValidation',
                 'note', 'textFormatRuns', 'chipRuns')


def execute(request, project):
    """Add setup guidance without logging credentials or request headers."""
    try:
        return request.execute()
    except HttpError as exc:
        try:
            error = json.loads(exc.content).get('error', {})
        except (ValueError, TypeError):
            raise exc
        reasons = {d.get('reason') for d in error.get('details', []) if isinstance(d, dict)}
        if 'SERVICE_DISABLED' in reasons:
            raise ValueError(
                f'Google Sheets API is disabled in the configured project {project}. '
                'Enable sheets.googleapis.com in that project, then retry the read. '
                'Drive access alone does not enable the Sheets service.'
            ) from exc
        raise


def _column(label):
    number = 0
    for letter in label:
        number = number * 26 + ord(letter) - ord('A') + 1
    return number - 1


def parse_range(a1):
    """Require an explicit sheet and finite cell rectangle; no whole columns."""
    if not isinstance(a1, str) or '!' not in a1:
        raise ValueError("Use a bounded range with a sheet name, such as 'Plans'!A2:C4")
    title, coordinates = a1.rsplit('!', 1)
    if title.startswith("'"):
        if not re.fullmatch(r"'(?:[^']|'')+'", title):
            raise ValueError('Invalid quoted sheet name')
        title = title[1:-1].replace("''", "'")
    elif not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', title):
        raise ValueError('Quote sheet names that contain spaces or punctuation')
    match = re.fullmatch(r'([A-Za-z]{1,3})([1-9][0-9]*)(?::([A-Za-z]{1,3})([1-9][0-9]*))?', coordinates)
    if not match:
        raise ValueError('Use explicit cell coordinates, not whole rows or columns')
    first_col, first_row, last_col, last_row = match.groups()
    first_col = first_col.upper()
    last_col = (last_col or first_col).upper()
    last_row = last_row or first_row
    start_row, end_row = int(first_row) - 1, int(last_row)
    start_col, end_col = _column(first_col), _column(last_col) + 1
    count = (end_row - start_row) * (end_col - start_col)
    if end_row <= start_row or end_col <= start_col or count > MAX_CELLS:
        raise ValueError(f'Range must contain 1–{MAX_CELLS} cells in forward order')
    quoted_title = "'" + title.replace("'", "''") + "'"
    normalized = f'{quoted_title}!{first_col}{first_row}:{last_col}{last_row}'
    return title, normalized, {
        'startRowIndex': start_row, 'endRowIndex': end_row,
        'startColumnIndex': start_col, 'endColumnIndex': end_col,
    }


def fingerprint(snapshot):
    cells = [[{k: cell[k] for k in STORED_FIELDS if k in cell}
              for cell in row] for row in snapshot['cells']]
    cells = deepcopy(cells)
    for row in cells:
        for cell in row:
            entered = cell.get('userEnteredValue', {})
            value = entered.get('numberValue')
            if isinstance(value, float) and math.isfinite(value) and value.is_integer():
                entered['numberValue'] = int(value)
    payload = {'spreadsheet_id': snapshot['spreadsheet_id'],
               'grid_range': snapshot['grid_range'], 'cells': cells}
    return hashlib.sha256(json.dumps(payload, sort_keys=True, ensure_ascii=False,
                                    separators=(',', ':')).encode()).hexdigest()


def read_cells(client, spreadsheet_id, a1, project):
    title, normalized, bounds = parse_range(a1)
    result = execute(client.spreadsheets().get(
        spreadsheetId=spreadsheet_id, ranges=[normalized],
        fields=('spreadsheetId,properties(title,locale,timeZone),'
                'sheets(properties(sheetId,title,gridProperties),'
                f'data(startRow,startColumn,rowData(values({CELL_FIELDS}))))'),
    ), project)
    tabs = [tab for tab in result.get('sheets', []) if tab['properties']['title'] == title]
    if len(tabs) != 1:
        raise ValueError('The exact sheet title could not be resolved')
    tab = tabs[0]
    grid = tab['properties']['gridProperties']
    if bounds['endRowIndex'] > grid['rowCount'] or bounds['endColumnIndex'] > grid['columnCount']:
        raise ValueError('The requested cells are outside the existing grid')
    height = bounds['endRowIndex'] - bounds['startRowIndex']
    width = bounds['endColumnIndex'] - bounds['startColumnIndex']
    cells = [[{} for _ in range(width)] for _ in range(height)]
    for block in tab.get('data', []):
        row_base = block.get('startRow', 0) - bounds['startRowIndex']
        col_base = block.get('startColumn', 0) - bounds['startColumnIndex']
        for r, row in enumerate(block.get('rowData', [])):
            for c, cell in enumerate(row.get('values', [])):
                if 0 <= row_base + r < height and 0 <= col_base + c < width:
                    cells[row_base + r][col_base + c] = cell
    if len(json.dumps(cells, ensure_ascii=False)) > MAX_TEXT * 4:
        raise ValueError('Range response is too large; read a smaller rectangle')
    snapshot = {'spreadsheet_id': spreadsheet_id, 'range': normalized,
                'properties': result.get('properties', {}),
                'grid_range': {'sheetId': tab['properties']['sheetId'], **bounds},
                'row_count': height, 'column_count': width, 'cells': cells}
    snapshot['sha256'] = fingerprint(snapshot)
    return snapshot


def _entered_value(value):
    if value is None:
        return None
    if isinstance(value, bool):
        return {'boolValue': value}
    if isinstance(value, (int, float)) and math.isfinite(value):
        return {'numberValue': value}
    if isinstance(value, str):
        if not value:
            return None
        return {'stringValue': value}
    raise ValueError('Cells must be strings, finite numbers, booleans or null')


def update_cells(client, spreadsheet_id, a1, values, expected_sha256, project):
    _, _, bounds = parse_range(a1)
    if not isinstance(expected_sha256, str) or not re.fullmatch(r'[0-9a-f]{64}', expected_sha256):
        raise ValueError('Pass sha256 from a fresh get_spreadsheet_cells read of the exact range')
    height = bounds['endRowIndex'] - bounds['startRowIndex']
    width = bounds['endColumnIndex'] - bounds['startColumnIndex']
    if (not isinstance(values, list) or len(values) != height
            or any(not isinstance(row, list) or len(row) != width for row in values)):
        raise ValueError('Value rows and columns must exactly match the bounded range')
    if len(json.dumps(values, ensure_ascii=False)) > MAX_TEXT:
        raise ValueError(f'Write payload exceeds {MAX_TEXT} characters; use a smaller range')
    entered = [[_entered_value(value) for value in row] for row in values]
    before = read_cells(client, spreadsheet_id, a1, project)
    if before['sha256'] != expected_sha256:
        raise ValueError('Cells or their structure changed since the last read; reread and reconcile')
    for row in before['cells']:
        for cell in row:
            if (cell.get('dataValidation') or cell.get('chipRuns') or cell.get('textFormatRuns')
                    or 'formulaValue' in cell.get('userEnteredValue', {})):
                raise ValueError('Target includes formulas, validation, chips or rich text; '
                                 'this content-only tool leaves those cells unchanged')
    expected = deepcopy(before)
    rows = []
    for r, row in enumerate(entered):
        new_row = []
        for c, value in enumerate(row):
            expected['cells'][r][c].pop('userEnteredValue', None)
            if value is not None:
                expected['cells'][r][c]['userEnteredValue'] = value
            new_row.append({'userEnteredValue': value} if value is not None else {})
        rows.append({'values': new_row})
    request = {'updateCells': {'range': before['grid_range'], 'rows': rows,
                               'fields': 'userEnteredValue'}}
    try:
        execute(client.spreadsheets().batchUpdate(
            spreadsheetId=spreadsheet_id, body={'requests': [request]}), project)
    except Exception as exc:
        raise RuntimeError('Write response failed; the save may have reached Google. '
                           'Read the range before retrying. ' + str(exc)) from exc
    try:
        after = read_cells(client, spreadsheet_id, a1, project)
    except Exception as exc:
        raise RuntimeError('Write was accepted, but read-back failed. '
                           'Read the range before retrying.') from exc
    verified = fingerprint(expected) == after['sha256']
    return {'spreadsheet_id': spreadsheet_id, 'range': after['range'],
            'updated_cells': height * width, 'verified': verified,
            'status': 'saved_and_verified' if verified else 'saved_but_readback_differs',
            'before_sha256': before['sha256'], 'after_sha256': after['sha256'],
            'note': ('Only cell values were written; cell formats and notes were checked. '
                     'The prior hash is a stale-write check, not an atomic lock.'),
            'read_back': after}
