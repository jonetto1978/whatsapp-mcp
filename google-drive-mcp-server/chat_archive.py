"""Bounded reads from saved chat files. This module never contacts WhatsApp."""
from datetime import date, datetime, time, timedelta
import base64
import binascii
import hashlib
import json
import re
from zoneinfo import ZoneInfo


def read_archive(manifest, load_month, start_date, end_date, limit=40,
                 cursor=None, query=None, include_transcripts=False, max_characters=12000):
    if manifest.get('schema') != 'whatsapp-chat-archive/v1':
        raise ValueError('File is not a supported chat manifest')
    if not re.fullmatch(r'\d{4}-\d{2}-\d{2}', start_date) or not re.fullmatch(r'\d{4}-\d{2}-\d{2}', end_date):
        raise ValueError('start_date and end_date must use YYYY-MM-DD')
    start, end = date.fromisoformat(start_date), date.fromisoformat(end_date)
    if start > end:
        raise ValueError('start_date must not be after end_date')
    tz = ZoneInfo(manifest.get('timezone', 'America/Argentina/Buenos_Aires'))
    lower = int(datetime.combine(start, time.min, tz).timestamp())
    upper = int(datetime.combine(end + timedelta(days=1), time.min, tz).timestamp())
    limit = max(1, min(int(limit), 100))
    max_characters = max(1, min(int(max_characters), 30000))
    phrase = (query or '').casefold()
    signature = hashlib.sha256(json.dumps([manifest['chat']['id'], start_date, end_date,
        phrase, include_transcripts, [(m['file_id'], m['sha256']) for m in manifest['months']]],
        sort_keys=True).encode()).hexdigest()[:20]
    after = None
    if cursor:
        try:
            if len(cursor) > 512 or not re.fullmatch(r'[A-Za-z0-9_-]+', cursor):
                raise ValueError('Invalid archive cursor')
            value = json.loads(base64.urlsafe_b64decode(cursor + '=' * (-len(cursor) % 4)))
            if value['scope'] != signature:
                raise ValueError('Cursor does not match this date range, query or archive version')
            after = (int(value['timestamp']), str(value['id']))
        except (KeyError, TypeError, json.JSONDecodeError, binascii.Error) as exc:
            raise ValueError('Invalid archive cursor') from exc

    rows, seen, fetched = [], set(), []
    source_characters = 0
    unread_months = False
    months = [m for m in sorted(manifest['months'], key=lambda m: m['month'])
              if start_date[:7] <= m['month'] <= end_date[:7]]
    for month_index, month in enumerate(months):
        if after and month['max_timestamp'] < after[0]:
            continue
        payload = load_month(month)
        if hashlib.sha256(payload).hexdigest() != month['sha256']:
            raise ValueError('Saved month changed; refresh its manifest before reading')
        text = payload.decode('utf-8')
        source_characters += len(text)
        fetched.append(month['month'])
        for line in text.splitlines():
            if not line.strip():
                continue
            record = json.loads(line)
            key = (int(record['timestamp']), record['id'])
            if record['id'] in seen or not lower <= key[0] < upper or (after and key <= after):
                continue
            seen.add(record['id'])
            plain = record.get('content_text') or ''
            voice = record.get('voice_note_transcript') or ''
            if phrase and phrase not in (plain + ('\n' + voice if include_transcripts else '')).casefold():
                continue
            item = {k: record[k] for k in ['id', 'timestamp', 'date', 'sender_display', 'is_from_me', 'type']}
            item['content_text'] = plain
            item['has_transcript'] = bool(voice)
            if include_transcripts:
                item['voice_note_transcript'] = voice
            for field in ('transcript_file_id', 'transcript_url', 'audio_file_id', 'audio_url'):
                if record.get(field):
                    item[field] = record[field]
            rows.append(item)
        rows.sort(key=lambda r: (r['timestamp'], r['id']))
        # Read later months only when this page still needs them. One extra
        # record establishes has_more; never load the whole archive by default.
        content_size = sum(len(r['content_text']) + len(r.get('voice_note_transcript', '')) for r in rows)
        if len(rows) > limit or (rows and content_size > max_characters):
            unread_months = month_index + 1 < len(months)
            break

    selected, returned_characters = [], 0
    for item in rows:
        if len(selected) >= limit:
            break
        size = len(item['content_text']) + len(item.get('voice_note_transcript', ''))
        remaining = max_characters - returned_characters
        if size > remaining:
            if selected:
                break
            item['text_truncated'] = True
            item['original_text_characters'] = size
            item['content_text'] = item['content_text'][:remaining]
            remaining -= len(item['content_text'])
            if 'voice_note_transcript' in item:
                item['voice_note_transcript'] = item['voice_note_transcript'][:remaining]
            size = max_characters
        selected.append(item)
        returned_characters += size
    more = len(rows) > len(selected) or unread_months
    next_cursor = None
    if more and selected:
        last = selected[-1]
        raw_cursor = json.dumps({'scope': signature, 'timestamp': last['timestamp'], 'id': last['id']}, separators=(',', ':'))
        next_cursor = base64.urlsafe_b64encode(raw_cursor.encode()).decode().rstrip('=')
    return {'chat': manifest['chat']['display_name'], 'requested_dates': [start_date, end_date],
        'timezone': str(tz), 'records': selected, 'record_count': len(selected),
        'returned_text_characters': returned_characters, 'source_file_characters_read': source_characters,
        'months_read': fetched, 'has_more': more, 'next_cursor': next_cursor,
        'saved_coverage': manifest['coverage'], 'whatsapp_requests': 0,
        'history_policy': 'No range expansion. Older or newer history requires an explicit user request.'}
