from copy import deepcopy
import unittest

from sheets import fingerprint, parse_range, read_cells, update_cells


class Request:
    def __init__(self, run):
        self.run = run

    def execute(self):
        return self.run()


class FakeSheets:
    def __init__(self):
        self.cells = [[{} for _ in range(8)] for _ in range(20)]
        self.cells[1][0] = {'userEnteredValue': {'stringValue': 'Teacher note'},
                            'userEnteredFormat': {'wrapStrategy': 'WRAP'},
                            'note': 'Keep this note'}
        self.cells[2][0] = {'userEnteredFormat': {'wrapStrategy': 'WRAP'}}
        self.cells[1][1] = {'userEnteredValue': {'formulaValue': '=C2+1'}}
        self.writes = []
        self.fail_write = False
        self.alter_after_write = False

    def spreadsheets(self):
        return self

    def get(self, **kwargs):
        def run():
            _, _, b = parse_range(kwargs['ranges'][0])
            rows = [{'values': deepcopy(row[b['startColumnIndex']:b['endColumnIndex']])}
                    for row in self.cells[b['startRowIndex']:b['endRowIndex']]]
            return {'spreadsheetId': 'file', 'properties': {'title': 'Course'},
                    'sheets': [{'properties': {'sheetId': 7, 'title': "Team's Notes",
                                              'gridProperties': {'rowCount': 20, 'columnCount': 8}},
                                'data': [{'startRow': b['startRowIndex'],
                                          'startColumn': b['startColumnIndex'], 'rowData': rows}]}]}
        return Request(run)

    def batchUpdate(self, **kwargs):
        def run():
            self.writes.append(deepcopy(kwargs))
            request = kwargs['body']['requests'][0]['updateCells']
            if self.fail_write:
                raise TimeoutError('Response lost')
            b = request['range']
            for r, row in enumerate(request['rows']):
                for c, cell in enumerate(row['values']):
                    target = self.cells[b['startRowIndex'] + r][b['startColumnIndex'] + c]
                    target.pop('userEnteredValue', None)
                    if 'userEnteredValue' in cell:
                        target['userEnteredValue'] = cell['userEnteredValue']
            if self.alter_after_write:
                self.cells[b['startRowIndex']][b['startColumnIndex']]['note'] = 'Changed by another editor'
            return {'replies': [{}]}
        return Request(run)


class SheetsTests(unittest.TestCase):
    def setUp(self):
        self.api = FakeSheets()
        self.a1 = "'Team''s Notes'!A2:A3"

    def read(self, a1=None):
        return read_cells(self.api, 'file', a1 or self.a1, 'project')

    def write(self, values, token=None, a1=None):
        return update_cells(self.api, 'file', a1 or self.a1, values,
                            token or self.read(a1)['sha256'], 'project')

    def test_quoted_sheet_and_nonzero_offsets(self):
        result = self.read()
        self.assertEqual(result['grid_range'], {'sheetId': 7, 'startRowIndex': 1,
                                              'endRowIndex': 3, 'startColumnIndex': 0,
                                              'endColumnIndex': 1})
        self.assertEqual(result['cells'][0][0]['note'], 'Keep this note')
        self.assertNotIn('userEnteredValue', result['cells'][1][0])

    def test_content_only_edit_preserves_note_format_and_neighbor_formula(self):
        before = deepcopy(self.api.cells)
        result = self.write([['Teacher note\n\nDated status'], ['=this stays literal']])
        self.assertTrue(result['verified'])
        self.assertEqual(result['updated_cells'], 2)
        self.assertEqual(self.api.cells[1][0]['note'], before[1][0]['note'])
        self.assertEqual(self.api.cells[1][0]['userEnteredFormat'], before[1][0]['userEnteredFormat'])
        self.assertEqual(self.api.cells[1][1], before[1][1])
        self.assertEqual(self.api.cells[2][0]['userEnteredValue'], {'stringValue': '=this stays literal'})
        request = self.api.writes[0]['body']['requests'][0]['updateCells']
        self.assertEqual(request['fields'], 'userEnteredValue')

    def test_stale_value_and_note_each_block_write(self):
        for field, value in [('note', 'Another note'), ('userEnteredValue', {'stringValue': 'New value'})]:
            with self.subTest(field=field):
                self.api = FakeSheets()
                token = self.read()['sha256']
                self.api.cells[1][0][field] = value
                with self.assertRaisesRegex(ValueError, 'changed since'):
                    self.write([['a'], ['b']], token)
                self.assertEqual(self.api.writes, [])

    def test_token_cannot_be_reused_for_another_file_or_range(self):
        original = self.read()
        other = deepcopy(original)
        other['spreadsheet_id'] = 'another-file'
        self.assertNotEqual(fingerprint(other), original['sha256'])
        other = deepcopy(original)
        other['grid_range']['sheetId'] = 8
        self.assertNotEqual(fingerprint(other), original['sha256'])

    def test_display_only_recalculation_does_not_invalidate_stored_cells(self):
        original = self.read()
        other = deepcopy(original)
        other['cells'][0][0]['formattedValue'] = 'Display changed'
        self.assertEqual(fingerprint(other), original['sha256'])

    def test_equivalent_json_number_formats_have_same_fingerprint(self):
        original = self.read()
        original['cells'][0][0]['userEnteredValue'] = {'numberValue': 12.0}
        other = deepcopy(original)
        other['cells'][0][0]['userEnteredValue'] = {'numberValue': 12}
        self.assertEqual(fingerprint(other), fingerprint(original))

    def test_protected_structures_are_not_overwritten(self):
        cases = [{'userEnteredValue': {'formulaValue': '=1+2'}},
                 {'dataValidation': {'condition': {'type': 'BOOLEAN'}}},
                 {'chipRuns': [{'startIndex': 0}]},
                 {'textFormatRuns': [{'startIndex': 0, 'format': {'bold': True}}]}]
        for case in cases:
            with self.subTest(case=case):
                self.api = FakeSheets()
                self.api.cells[1][0].update(case)
                with self.assertRaisesRegex(ValueError, 'formulas, validation'):
                    self.write([['a'], ['b']])
                self.assertEqual(self.api.writes, [])

    def test_invalid_ranges_do_not_allow_unbounded_or_reversed_writes(self):
        for value in ['A1:A2', 'Sheet1!A:A', 'Sheet1!2:4', 'Sheet1!A0:A1',
                      'Sheet1!A3:A1', 'Sheet1!B1:A2', 'Sheet1!A1:ZZ100',
                      "'Broken!A1", 'Space Name!A1:A2']:
            with self.subTest(value=value), self.assertRaises(ValueError):
                parse_range(value)

    def test_wrong_shape_and_nonfinite_values_cannot_write(self):
        for values in [[['one']], [['one', 'two'], ['three', 'four']],
                       [[float('nan')], ['x']], [[{'unexpected': 'object'}], ['x']]]:
            with self.subTest(values=values), self.assertRaises(ValueError):
                self.write(values)
        self.assertEqual(self.api.writes, [])

    def test_grid_bounds_are_enforced(self):
        with self.assertRaisesRegex(ValueError, 'outside the existing grid'):
            self.read("'Team''s Notes'!A19:A22")

    def test_null_clears_only_the_cell_value(self):
        result = self.write([[None], [True]])
        self.assertTrue(result['verified'])
        self.assertNotIn('userEnteredValue', self.api.cells[1][0])
        self.assertEqual(self.api.cells[1][0]['note'], 'Keep this note')
        self.assertEqual(self.api.cells[2][0]['userEnteredValue'], {'boolValue': True})

    def test_readback_difference_is_not_reported_as_verified(self):
        self.api.alter_after_write = True
        result = self.write([['a'], ['b']])
        self.assertFalse(result['verified'])
        self.assertEqual(result['status'], 'saved_but_readback_differs')

    def test_timeout_does_not_trigger_a_blind_retry(self):
        self.api.fail_write = True
        with self.assertRaisesRegex(RuntimeError, 'may have reached Google'):
            self.write([['a'], ['b']])
        self.assertEqual(len(self.api.writes), 1)


if __name__ == '__main__':
    unittest.main()
