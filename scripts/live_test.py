#!/usr/bin/env python3
"""Bounded, opt-in SenseNova integration tests; credentials stay in memory.
Run after building bin/gateway. Uses SENSENOVA_LIVE_KEY or a hidden prompt.
"""
import argparse
import base64
import datetime as dt
import getpass
import hashlib
import json
import os
import pathlib
import secrets
import signal
import socket
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zlib

ROOT = pathlib.Path(__file__).resolve().parents[1]
UPSTREAM = 'https://token.sensenova.cn'


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--report', default='docs/live-test-2026-10-17.json')
    parser.add_argument('--images', action='store_true', help='Also perform real image generation/edit calls (consumes quota)')
    parser.add_argument('--only', action='append', default=[], help='Only run generation checks with this name prefix; can be repeated')
    args = parser.parse_args()
    key = os.environ.get('SENSENOVA_LIVE_KEY') or getpass.getpass('SenseNova API key (hidden, not saved): ')
    client_key, admin_key = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
    report = {'started_at': dt.datetime.now(dt.timezone(dt.timedelta(hours=8))).isoformat(), 'upstream': UPSTREAM, 'checks': [], 'limitations': ['Only one real account supplied: independent multi-account rotation remains covered by mock tests.', 'No documented official API-key balance endpoint; no fabricated remaining quota.'], 'generated_images_downloaded': False}
    expected_tokens = 0
    expected_requests = 0
    unknown_usage = 0
    gateway = None
    proc = None

    def safe(value):
        s = str(value)
        for secret in (key, client_key, admin_key):
            s = s.replace(secret, '[REDACTED]')
        return s

    def record(name, outcome, **details):
        check = {'name': name, 'outcome': outcome, **details}
        check = json.loads(safe(json.dumps(check, ensure_ascii=False)))
        report['checks'].append(check)
        print(f'{outcome.upper()}: {name} ' + safe(json.dumps(details, ensure_ascii=False)), flush=True)

    def request(origin, path, body=None, *, admin=False, auth=True, stream=False, method=None, headers=None, timeout=90):
        credential = key if origin == UPSTREAM else admin_key if admin else client_key
        h = {'Content-Type': 'application/json'}
        if auth:
            h['Authorization'] = 'Bearer ' + credential
        h.update(headers or {})
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(origin + path, data=data, headers=h, method=method)
        started = time.monotonic()
        first_data = None
        try:
            response = urllib.request.urlopen(req, timeout=timeout)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            status, response_headers = response.status, dict(response.headers)
            if stream and status == 200:
                chunks = []
                while True:
                    line = response.readline()
                    if not line:
                        break
                    if line.startswith(b'data:') and first_data is None:
                        first_data = time.monotonic() - started
                    chunks.append(line)
                raw = b''.join(chunks)
            else:
                raw = response.read()
        try:
            obj = json.loads(raw)
        except (ValueError, UnicodeDecodeError):
            obj = None
        return {'status': status, 'headers': response_headers, 'raw': raw, 'json': obj, 'elapsed_seconds': round(time.monotonic() - started, 3), 'first_data_seconds': round(first_data, 3) if first_data is not None else None}

    def usage(obj):
        u = obj.get('usage') if isinstance(obj, dict) else None
        if not isinstance(u, dict):
            return None
        # Same accounting semantics as the gateway: cumulative values, not deltas.
        inp = u.get('prompt_tokens', u.get('input_tokens', 0))
        out = u.get('completion_tokens', u.get('output_tokens', 0))
        if 'cache_read_input_tokens' in u:
            inp += u.get('cache_read_input_tokens', 0) + u.get('cache_creation_input_tokens', 0)
        return {'input_tokens': inp, 'output_tokens': out, 'total_tokens': u.get('total_tokens', inp + out)}

    def parse_sse(raw):
        events = []
        normalized = raw.replace(b'\r\n', b'\n')
        for frame in normalized.split(b'\n\n'):
            payload = b'\n'.join(line[5:].lstrip(b' ') for line in frame.split(b'\n') if line.startswith(b'data:'))
            if payload == b'[DONE]':
                events.append({'type': 'done'})
            elif payload:
                try:
                    events.append(json.loads(payload))
                except ValueError:
                    pass
        result = {'input_tokens': 0, 'output_tokens': 0, 'total_tokens': 0}
        known = False
        for event in events:
            obj = event.get('response', event.get('message', event))
            u = usage(obj)
            if u:
                raw_u = obj['usage']
                if 'input_tokens' in raw_u or 'prompt_tokens' in raw_u:
                    result['input_tokens'] = u['input_tokens']
                if 'output_tokens' in raw_u or 'completion_tokens' in raw_u:
                    result['output_tokens'] = u['output_tokens']
                result['total_tokens'] = raw_u.get('total_tokens', result['input_tokens'] + result['output_tokens'])
                known = True
        terminal = any(e.get('type') in ('done', 'message_stop', 'response.completed', 'response.incomplete', 'response.failed') for e in events)
        failed = any(e.get('type') in ('error', 'response.failed') for e in events)
        return events, result if known else None, terminal and not failed

    def generation(name, path, body, *, stream=False, validate=None, headers=None):
        nonlocal expected_requests, expected_tokens, unknown_usage
        if args.only and not any(name.startswith(prefix) for prefix in args.only):
            record(name, 'skip', reason='not selected by --only')
            return None
        try:
            response = request(gateway, path, body, stream=stream, headers=headers, timeout=180)
        except Exception as error:
            expected_requests += 1
            unknown_usage += 1
            record(name, 'fail', error=safe(error))
            return None
        obj = response['json']
        local_rejection = isinstance(obj, dict) and (obj.get('error') or {}).get('type') == 'gateway_error'
        if local_rejection:
            record(name, 'skip', status=response['status'], reason=obj['error'])
            return response
        expected_requests += 1
        if stream and response['status'] == 200:
            events, u, valid = parse_sse(response['raw'])
            details = {'events': len(events), 'terminal': valid, 'first_data_seconds': response['first_data_seconds']}
        else:
            u = usage(obj)
            valid = bool(obj) and (validate(obj) if validate else True)
            details = {}
        if u:
            expected_tokens += u['total_tokens']
        else:
            unknown_usage += 1
        ok = response['status'] == 200 and valid
        if not ok:
            details['error'] = (obj or {'body': response['raw'][:1000].decode(errors='replace')})
        record(name, 'pass' if ok else 'blocked' if response['status'] in (403, 429) else 'fail', status=response['status'], elapsed_seconds=response['elapsed_seconds'], usage=u, **details)
        return response

    try:
        direct = request(UPSTREAM, '/v1/models')
        if direct['status'] != 200 or not isinstance(direct['json'], dict) or 'data' not in direct['json']:
            record('direct-model-discovery', 'fail', status=direct['status'], error=direct['json'])
            return
        models = direct['json']['data']
        report['models'] = models
        record('direct-model-discovery', 'pass', status=200, models=[m['id'] for m in models], elapsed_seconds=direct['elapsed_seconds'])
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        gateway = f'http://127.0.0.1:{port}'
        with tempfile.TemporaryDirectory(prefix='daydaynew-live-') as temp:
            temp = pathlib.Path(temp)
            config = {'listen': f'127.0.0.1:{port}', 'state_file': str(temp / 'state.json'), 'refresh_interval_seconds': 3600, 'request_timeout_seconds': 180, 'max_attempts': 1, 'accounts': [{'id': 'sense-live', 'api_key_env': 'SENSENOVA_LIVE_KEY', 'max_in_flight': 4}]}
            (temp / 'config.json').write_text(json.dumps(config))
            env = dict(os.environ, SENSENOVA_LIVE_KEY=key, GATEWAY_API_KEY=client_key, GATEWAY_ADMIN_KEY=admin_key)
            proc = subprocess.Popen([str(ROOT / 'bin/gateway'), '-config', str(temp / 'config.json')], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            for _ in range(100):
                try:
                    if request(gateway, '/healthz', auth=False, timeout=1)['status'] == 200:
                        break
                except OSError:
                    time.sleep(.05)
            else:
                raise RuntimeError('Gateway failed to start')
            record('gateway-startup', 'pass')
            proxied_models = request(gateway, '/v1/models')
            got_ids = sorted(m['id'] for m in proxied_models['json'].get('data', [])) if isinstance(proxied_models['json'], dict) else []
            record('proxied-model-discovery', 'pass' if got_ids == sorted(m['id'] for m in models) else 'fail', status=proxied_models['status'], count=len(got_ids))
            checks = [('/v1/models', False, False, 401), ('/admin/v1/accounts', False, True, 401), ('/metrics', False, False, 401)]
            for path, admin, auth, want in checks:
                result = request(gateway, path, admin=admin, auth=auth)
                record('auth-isolation:' + path, 'pass' if result['status'] == want else 'fail', status=result['status'])
            chat_models = [m for m in models if 'text' in m.get('output_modalities', ['text'])]
            preferred = next((m['id'] for m in chat_models if m['id'] == 'sensenova-6.8-flash-lite'), chat_models[0]['id'] if chat_models else None)
            chat_models.sort(key=lambda m: (m['id'] != preferred, m['id']))
            for model in chat_models:
                generation('chat:' + model['id'], '/v1/chat/completions', {'model': model['id'], 'messages': [{'role': 'user', 'content': 'Reply with exactly OK.'}], 'max_tokens': 32, 'reasoning_effort': 'none'}, validate=lambda o: bool(o.get('choices')))
            if preferred:
                generation('chat-sse', '/v1/chat/completions', {'model': preferred, 'messages': [{'role': 'user', 'content': 'Reply with exactly OK.'}], 'max_tokens': 32, 'reasoning_effort': 'none', 'stream': True, 'stream_options': {'include_usage': True}}, stream=True)
                generation('responses', '/v1/responses', {'model': preferred, 'input': 'Reply with exactly OK.', 'max_output_tokens': 32, 'reasoning': {'effort': 'none'}}, validate=lambda o: o.get('object') == 'response' and bool(o.get('output')))
                generation('responses-sse', '/v1/responses', {'model': preferred, 'input': 'Reply with exactly OK.', 'max_output_tokens': 32, 'reasoning': {'effort': 'none'}, 'stream': True}, stream=True)
                messages = {'model': preferred, 'max_tokens': 32, 'messages': [{'role': 'user', 'content': 'Reply with exactly OK.'}], 'thinking': {'type': 'disabled'}}
                generation('messages', '/v1/messages', messages, validate=lambda o: o.get('type') == 'message' and bool(o.get('content')))
                generation('messages-sse', '/v1/messages', {**messages, 'stream': True}, stream=True)
                generation('explicit-provider-route', '/providers/sensenova/v1/chat/completions', {'model': preferred, 'messages': [{'role': 'user', 'content': 'OK'}], 'max_tokens': 8, 'reasoning_effort': 'none'})
                tools = [{'type': 'function', 'function': {'name': 'return_ok', 'description': 'Return OK.', 'parameters': {'type': 'object', 'properties': {'value': {'type': 'string'}}, 'required': ['value']}}}]
                generation('tool-calling', '/v1/chat/completions', {'model': preferred, 'messages': [{'role': 'user', 'content': 'Use return_ok with value OK.'}], 'tools': tools, 'tool_choice': {'type': 'function', 'function': {'name': 'return_ok'}}, 'max_tokens': 64, 'reasoning_effort': 'none'}, validate=lambda o: bool(o.get('choices', [{}])[0].get('message', {}).get('tool_calls')))
                # A tiny valid synthetic PNG avoids third-party URLs and personal data.
                def chunk(typ, payload):
                    return struct.pack('>I', len(payload)) + typ + payload + struct.pack('>I', zlib.crc32(typ + payload) & 0xffffffff)
                png = b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', 256, 256, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress((b'\0' + b'\xff\0\0' * 256) * 256)) + chunk(b'IEND', b'')
                image_data = 'data:image/png;base64,' + base64.b64encode(png).decode()
                generation('vision-data-url', '/v1/chat/completions', {'model': preferred, 'messages': [{'role': 'user', 'content': [{'type': 'text', 'text': 'Name the main color in one word.'}, {'type': 'image_url', 'image_url': {'url': image_data}}]}], 'max_tokens': 32, 'reasoning_effort': 'none'}, validate=lambda o: bool(o.get('choices')))
                if args.images:
                    image_models = [m['id'] for m in models if 'image' in m.get('output_modalities', [])]
                    for model in image_models:
                        generation('image-generation:' + model, '/v1/images/generations', {'model': model, 'prompt': 'A plain red circle centered on a white background.', 'n': 1, 'size': '512x512', 'response_format': 'url', 'output_format': 'png', 'watermark': True, 'prompt_extend': False}, validate=lambda o: bool(o.get('data')))
                    if image_models:
                        image_model = next((m for m in image_models if m.endswith('fast')), image_models[0])
                        generation('image-edit-json', '/v1/images/edits', {'model': image_model, 'images': [{'image_url': image_data}], 'prompt': 'Change the solid red square to blue.', 'n': 1, 'size': '512x512', 'response_format': 'url', 'output_format': 'png', 'watermark': True, 'prompt_extend': False}, validate=lambda o: bool(o.get('data')))
            # Error pass-through uses a non-generating invalid request.
            invalid = request(gateway, '/v1/chat/completions', {'messages': []})
            if not isinstance(invalid['json'], dict) or (invalid['json'].get('error') or {}).get('type') != 'gateway_error':
                expected_requests += 1
                unknown_usage += 1
            record('upstream-validation-error', 'pass' if invalid['status'] == 400 and 'error' in (invalid['json'] or {}) else 'fail', status=invalid['status'], response=invalid['json'])
            # Wait until the final handler has committed its accounting.
            for _ in range(100):
                state = request(gateway, '/admin/v1/accounts', admin=True)['json']['data'][0]
                if state['in_flight'] == 0:
                    break
                time.sleep(.02)
            stats = state['stats']
            record('usage-reconciliation', 'pass' if stats['total_tokens'] == expected_tokens and stats['requests'] == expected_requests and stats['requests_without_usage'] == unknown_usage else 'fail', expected_tokens=expected_tokens, expected_requests=expected_requests, expected_unknown_usage=unknown_usage, actual=stats)
            record('quota-unknown-is-explicit', 'pass' if state['quota'] is None and state['quota_status'] == 'unsupported' else 'fail', quota_status=state['quota_status'])
            observed_denials = state.get('unavailable_models', {})
            catalog = request(gateway, '/v1/models')['json']
            catalog_ids = [m['id'] for m in catalog.get('data', [])]
            record('observed-permission-filter', 'pass' if not set(observed_denials).intersection(catalog_ids) else 'fail', excluded_models=list(observed_denials), advertised_models=catalog_ids)
            providers = request(gateway, '/admin/v1/providers', admin=True)
            record('provider-discovery', 'pass' if providers['status'] == 200 and 'sensenova' in providers['json'].get('data', []) else 'fail')
            invalid_quota = request(gateway, '/admin/v1/accounts/sense-live/quota', {'source': 'integration-test-synthetic'}, admin=True, method='PUT')
            record('invalid-quota-rejected', 'pass' if invalid_quota['status'] == 400 else 'fail', status=invalid_quota['status'])
            now = dt.datetime.now(dt.timezone.utc)
            synthetic_quota = {'source': 'integration-test-synthetic', 'observed_at': now.isoformat(), 'valid_until': (now + dt.timedelta(minutes=1)).isoformat(), 'remaining_tokens': None, 'buckets': [{'name': 'synthetic-window', 'pool': 'synthetic', 'unit': 'credits', 'remaining': 123, 'limit': 1000}]}
            set_quota = request(gateway, '/admin/v1/accounts/sense-live/quota', synthetic_quota, admin=True, method='PUT')
            quota_state = request(gateway, '/admin/v1/accounts', admin=True)['json']['data'][0]
            quota_metrics = request(gateway, '/metrics', admin=True)['raw'].decode()
            record('quota-snapshot-and-metrics', 'pass' if set_quota['status'] == 200 and quota_state['quota_status'] == 'reported' and 'gateway_quota_remaining{' in quota_metrics else 'fail', source='integration-test-synthetic; not official balance')
            clear_quota = request(gateway, '/admin/v1/accounts/sense-live/quota', admin=True, method='DELETE')
            record('quota-snapshot-clear', 'pass' if clear_quota['status'] == 200 else 'fail')
            metrics = request(gateway, '/metrics', admin=True)
            metrics_text = metrics['raw'].decode()
            record('prometheus', 'pass' if metrics['status'] == 200 and 'gateway_tokens_total' in metrics_text and 'gateway_remaining_tokens{' not in metrics_text else 'fail', status=metrics['status'])
            for enabled in (False, True):
                result = request(gateway, '/admin/v1/accounts/sense-live/enabled', {'enabled': enabled}, admin=True, method='PUT')
                record('account-' + ('enable' if enabled else 'disable'), 'pass' if result['status'] == 200 else 'fail')
                if not enabled:
                    blocked = request(gateway, '/v1/chat/completions', {'model': preferred, 'messages': []})
                    record('disabled-account-is-not-used', 'pass' if blocked['status'] == 503 else 'fail', status=blocked['status'])
            proc.send_signal(signal.SIGTERM)
            _, logs = proc.communicate(timeout=20)
            record('graceful-shutdown', 'pass' if proc.returncode == 0 else 'fail', exit_code=proc.returncode)
            persisted = json.loads((temp / 'state.json').read_text())
            report['account_state'] = state
            record('persistent-accounting', 'pass' if persisted['accounts'][0]['stats']['total_tokens'] == expected_tokens and key not in json.dumps(persisted) else 'fail', total_tokens=persisted['accounts'][0]['stats']['total_tokens'])
            proc = None
    except Exception as error:
        record('test-runner', 'fail', error=safe(error))
    finally:
        if proc is not None and proc.poll() is None:
            proc.send_signal(signal.SIGTERM)
            try:
                proc.communicate(timeout=20)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.communicate()
        report['finished_at'] = dt.datetime.now(dt.timezone(dt.timedelta(hours=8))).isoformat()
        report['summary'] = {s: sum(c['outcome'] == s for c in report['checks']) for s in ('pass', 'fail', 'blocked', 'skip')}
        target = ROOT / args.report
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(safe(json.dumps(report, ensure_ascii=False, indent=2)) + '\n')
        print('REPORT: ' + str(target), flush=True)
        print('SUMMARY: ' + json.dumps(report['summary']), flush=True)
    return 1 if report['summary']['fail'] else 0


if __name__ == '__main__':
    raise SystemExit(main())
