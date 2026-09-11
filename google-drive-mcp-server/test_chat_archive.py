from datetime import datetime
import hashlib
import json
import unittest

from chat_archive import read_archive


class ArchiveTests(unittest.TestCase):
    def setUp(self):
        self.payloads = {}
        self.loaded = []
        self.manifest = {'schema': 'whatsapp-chat-archive/v1',
            'chat': {'id': 'chat-a', 'display_name': 'Example'},
            'timezone': 'America/Argentina/Buenos_Aires',
            'coverage': {'complete_server_history': False}, 'months': []}
        for month, records in [('2026-08', [('old', 31, 'older', '')]),
                               ('2026-09', [('a', 1, 'one', ''), ('b', 10, 'two', 'voice text'),
                                            ('c', 10, 'three', ''), ('d', 11, 'later', '')]),
                               ('2026-10', [('future', 1, 'next month', '')])]:
            rows = []
            for identifier, day, text, transcript in records:
                date = f'{month}-{day:02d}T12:00:00-03:00'
                rows.append({'id': identifier, 'timestamp': int(datetime.fromisoformat(date).timestamp()),
                    'date': date, 'sender_display': 'Person', 'is_from_me': False, 'type': 'text',
                    'content_text': text, 'voice_note_transcript': transcript})
            payload = ''.join(json.dumps(r) + '\n' for r in rows).encode()
            self.payloads[month] = payload
            self.manifest['months'].append({'month': month, 'file_id': month,
                'sha256': hashlib.sha256(payload).hexdigest(),
                'max_timestamp': max(r['timestamp'] for r in rows)})

    def load(self, month):
        self.loaded.append(month['month'])
        return self.payloads[month['month']]

    def read(self, start='2026-09-10', end='2026-09-10', **kwargs):
        return read_archive(self.manifest, self.load, start, end, **kwargs)

    def test_reads_only_requested_month_and_dates(self):
        result = self.read()
        self.assertEqual(self.loaded, ['2026-09'])
        self.assertEqual([r['id'] for r in result['records']], ['b', 'c'])
        self.assertEqual(result['whatsapp_requests'], 0)
        self.assertFalse(result['saved_coverage']['complete_server_history'])

    def test_cursor_no_duplicates_and_scope_bound(self):
        first = self.read(limit=1)
        self.assertRegex(first['next_cursor'], r'^[A-Za-z0-9_-]+$')
        second = self.read(limit=1, cursor=first['next_cursor'])
        self.assertEqual(first['records'][0]['id'], 'b')
        self.assertEqual(second['records'][0]['id'], 'c')
        self.assertFalse(second['has_more'])
        with self.assertRaises(ValueError):
            self.read(end='2026-09-11', cursor=first['next_cursor'])

    def test_voice_is_opt_in_and_query_uses_selected_text(self):
        self.assertNotIn('voice_note_transcript', self.read()['records'][0])
        self.assertEqual(self.read(query='VOICE')['records'], [])
        result = self.read(query='VOICE', include_transcripts=True)
        self.assertEqual([r['id'] for r in result['records']], ['b'])

    def test_character_budget_preserves_next_record(self):
        first = self.read(max_characters=3)
        self.assertEqual(first['returned_text_characters'], 3)
        second = self.read(cursor=first['next_cursor'], max_characters=3)
        self.assertTrue(second['records'][0]['text_truncated'])
        self.assertEqual(second['records'][0]['content_text'], 'thr')

    def test_page_does_not_fetch_later_months(self):
        result = self.read('2026-08-01', '2026-10-31', limit=1)
        self.assertEqual(self.loaded, ['2026-08', '2026-09'])
        self.assertTrue(result['has_more'])
        self.assertNotIn('2026-10', self.loaded)

    def test_truncated_single_record_keeps_later_month_cursor(self):
        result = self.read('2026-08-01', '2026-10-31', max_characters=1)
        self.assertEqual(self.loaded, ['2026-08'])
        self.assertTrue(result['has_more'])
        self.assertIsNotNone(result['next_cursor'])

    def test_changed_month_and_invalid_dates_fail(self):
        self.payloads['2026-09'] += b' '
        with self.assertRaises(ValueError):
            self.read()
        with self.assertRaises(ValueError):
            self.read('2026-09-11', '2026-09-10')

    def test_empty_period_does_not_expand(self):
        result = self.read('2026-07-01', '2026-07-31')
        self.assertEqual(self.loaded, [])
        self.assertEqual(result['record_count'], 0)
        self.assertFalse(result['has_more'])


if __name__ == '__main__':
    unittest.main()
