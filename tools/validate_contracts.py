#!/usr/bin/env python3
# /// script
# requires-python = ">=3.11"
# dependencies = ["jsonschema>=4.18", "pyyaml>=6"]
# ///
"""Validate the LensDock data contracts (schema, OpenAPI, examples, coordinates); starts no container or browser."""
from __future__ import annotations

import argparse
import copy
import json
import math
import random
import re
import shutil
import subprocess
import tempfile
import unittest
import xml.etree.ElementTree as ET
from pathlib import Path
from urllib.parse import unquote, urldefrag
from typing import Any

import yaml
from jsonschema import Draft202012Validator, FormatChecker, ValidationError

ROOT = Path(__file__).resolve().parents[1]
SCHEMA = json.loads((ROOT / 'schemas/feedback.schema.json').read_text(encoding='utf-8'))
EXAMPLE = json.loads((ROOT / 'examples/feedback.example.json').read_text(encoding='utf-8'))
OPENAPI = yaml.safe_load((ROOT / 'contracts/openapi.yaml').read_text(encoding='utf-8'))
VALIDATOR = Draft202012Validator(SCHEMA, format_checker=FormatChecker())


def validate_feedback(value: dict[str, Any]) -> None:
    """Validate the schema and selected cross-field product constraints."""
    VALIDATOR.validate(value)
    capture = value['capture']
    annotation_ids: set[str] = set()
    for annotation in value['annotations']:
        if annotation['id'] in annotation_ids:
            raise ValueError('Duplicate annotation ID.')
        annotation_ids.add(annotation['id'])
        if annotation['kind'] in ('rectangle', 'redact', 'ellipse', 'diamond', 'highlight', 'spotlight', 'blur', 'pixelate'):
            geometry = annotation['geometry']
            if geometry['x'] + geometry['width'] > 1 + 1e-12:
                raise ValueError('Rectangle extends beyond the image width.')
            if geometry['y'] + geometry['height'] > 1 + 1e-12:
                raise ValueError('Rectangle extends beyond the image height.')
    artifacts = {item['name']: item for item in value['artifacts']}
    if len(artifacts) != len(value['artifacts']):
        raise ValueError('Duplicate artifact name.')
    required = {'screenshot.redacted.png', 'screenshot.annotated.png'}
    if not required.issubset(artifacts):
        raise ValueError('Both redacted and annotated images are required.')
    for name, item in artifacts.items():
        expected_mime = 'image/png' if name.endswith('.png') else 'application/json'
        if item['mimeType'] != expected_mime:
            raise ValueError('Artifact MIME type does not match its declared name.')
    if artifacts['screenshot.redacted.png']['sha256'] != capture['redactedImageSha256']:
        raise ValueError('Capture and redacted image hashes differ.')
    includes_diagnostics = 'diagnostics.redacted.json' in artifacts
    if includes_diagnostics != value['sharePolicy']['diagnosticsIncluded']:
        raise ValueError('Diagnostic sharing declaration does not match artifacts.')
    if capture['source'] == 'page-viewport':
        page = capture['page']
        actual_x = capture['imageWidthPx'] / page['viewportWidthCss']
        actual_y = capture['imageHeightPx'] / page['viewportHeightCss']
        if not math.isclose(actual_x, page['pixelsPerCssX'], rel_tol=1e-6):
            raise ValueError('Incorrect horizontal pixel-to-CSS scale.')
        if not math.isclose(actual_y, page['pixelsPerCssY'], rel_tol=1e-6):
            raise ValueError('Incorrect vertical pixel-to-CSS scale.')


def normalized_pointer(client_x: float, client_y: float, left: float, top: float,
                       display_width: float, display_height: float) -> tuple[float, float]:
    """Map a pointer within the image-content rectangle, excluding letterboxing."""
    values = (client_x, client_y, left, top, display_width, display_height)
    if not all(math.isfinite(value) for value in values):
        raise ValueError('Coordinates must be finite.')
    if display_width <= 0 or display_height <= 0:
        raise ValueError('Display dimensions must be positive.')
    x = (client_x - left) / display_width
    y = (client_y - top) / display_height
    if not (0 <= x <= 1 and 0 <= y <= 1):
        raise ValueError('Pointer lies outside image content.')
    return x, y


def viewport_css(x: float, y: float, capture: dict[str, Any]) -> tuple[float, float]:
    """Convert only a viewport capture; never infer a DOM point from a desktop."""
    if capture['source'] != 'page-viewport':
        raise ValueError('Only page-viewport captures have this conversion.')
    if not (math.isfinite(x) and math.isfinite(y) and 0 <= x <= 1 and 0 <= y <= 1):
        raise ValueError('Invalid normalized point.')
    page = capture['page']
    return (x * capture['imageWidthPx'] / page['pixelsPerCssX'],
            y * capture['imageHeightPx'] / page['pixelsPerCssY'])


def drawing_example() -> dict[str, Any]:
    """A 0.2.0 standalone-drawing revision derived from the synthetic example."""
    value = copy.deepcopy(EXAMPLE)
    value['schemaVersion'] = '0.2.0'
    value['capture'].update(source='drawing', provenance='generated-canvas', runtimeGeneration='',
                            targetId=None, navigationId=None, consistency='unknown')
    value['capture'].pop('page')
    return value


def walk_refs(node: Any):
    if isinstance(node, dict):
        for key, value in node.items():
            if key == '$ref':
                yield value
            else:
                yield from walk_refs(value)
    elif isinstance(node, list):
        for value in node:
            yield from walk_refs(value)


def validate_local_reference(reference: str, source: Path) -> None:
    """Check local JSON pointers without fetching remote resources."""
    location, fragment = urldefrag(reference)
    if '://' in location or location.startswith('urn:'):
        raise ValueError('Unexpected external schema reference.')
    target = (source.parent / location).resolve() if location else source.resolve()
    if not target.is_relative_to(ROOT):
        raise ValueError('Reference escapes the repository.')
    if not target.is_file():
        raise ValueError(f'Missing reference target: {target}')
    content = target.read_text(encoding='utf-8')
    node: Any = json.loads(content) if target.suffix == '.json' else yaml.safe_load(content)
    if fragment:
        if not fragment.startswith('/'):
            raise ValueError('Only JSON pointer fragments are supported.')
        for part in fragment[1:].split('/'):
            key = unquote(part).replace('~1', '/').replace('~0', '~')
            node = node[int(key)] if isinstance(node, list) else node[key]


class ContractTests(unittest.TestCase):
    def test_schema_is_valid(self) -> None:
        Draft202012Validator.check_schema(SCHEMA)

    def test_published_example_is_valid(self) -> None:
        validate_feedback(EXAMPLE)

    def test_desktop_example_has_no_css_mapping(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        capture = value['capture']
        capture.update(source='desktop', provenance='client-framebuffer',
                       targetId=None, navigationId=None, consistency='best-effort')
        capture.pop('page')
        validate_feedback(value)
        with self.assertRaises(ValueError):
            viewport_css(.5, .5, capture)

    def test_raw_image_sharing_is_rejected(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['sharePolicy']['includesRawImage'] = True
        with self.assertRaises(ValidationError):
            validate_feedback(value)

    def test_unknown_script_fields_are_rejected(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['annotations'][0]['script'] = 'alert(1)'
        with self.assertRaises(ValidationError):
            validate_feedback(value)

    def test_out_of_bounds_rectangle_is_rejected(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['annotations'][0]['geometry'].update(x=.9, width=.2)
        with self.assertRaises(ValueError):
            validate_feedback(value)

    def test_duplicate_annotation_is_rejected(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['annotations'].append(copy.deepcopy(value['annotations'][0]))
        with self.assertRaises(ValueError):
            validate_feedback(value)

    def test_duplicate_artifact_is_rejected(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['artifacts'].append(copy.deepcopy(value['artifacts'][0]))
        with self.assertRaises(ValueError):
            validate_feedback(value)

    def test_path_traversal_artifact_name_is_rejected(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['artifacts'][0]['name'] = '../../profile/Cookies'
        with self.assertRaises(ValidationError):
            validate_feedback(value)

    def test_hash_mismatch_is_rejected(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['capture']['redactedImageSha256'] = 'c' * 64
        with self.assertRaises(ValueError):
            validate_feedback(value)

    def test_large_images_are_accepted(self) -> None:
        # No pixel, dimension or size quota is part of the contract.
        value = drawing_example()
        value['capture'].update(imageWidthPx=40000, imageHeightPx=40000)
        value['artifacts'][0]['sizeBytes'] = 200 * 1024 * 1024
        validate_feedback(value)

    def test_transparent_redaction_is_rejected(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['annotations'][2]['mode'] = 'blur'
        with self.assertRaises(ValidationError):
            validate_feedback(value)

    def test_drawing_example_is_valid_and_has_no_runtime_context(self) -> None:
        value = drawing_example()
        validate_feedback(value)
        with self.assertRaises(ValueError):
            viewport_css(.5, .5, value['capture'])

    def test_drawing_must_not_carry_runtime_or_page_context(self) -> None:
        for field, replacement in [('runtimeGeneration', 'gen-1'), ('targetId', 'target-1'),
                                   ('navigationId', 'nav-1'), ('provenance', 'client-framebuffer')]:
            value = drawing_example()
            value['capture'][field] = replacement
            with self.assertRaises(ValidationError, msg=field):
                validate_feedback(value)
        value = drawing_example()
        value['capture']['page'] = copy.deepcopy(EXAMPLE['capture']['page'])
        with self.assertRaises(ValidationError):
            validate_feedback(value)

    def test_other_sources_still_require_a_runtime_generation(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['capture']['runtimeGeneration'] = ''
        with self.assertRaises(ValidationError):
            validate_feedback(value)

    def test_schema_0_1_0_excludes_drawings_and_new_annotation_kinds(self) -> None:
        value = drawing_example()
        value['schemaVersion'] = '0.1.0'
        with self.assertRaises(ValidationError):
            validate_feedback(value)
        value = copy.deepcopy(EXAMPLE)
        value['annotations'].append({'id': 'e1', 'kind': 'ellipse', 'geometry': {'x': .1, 'y': .1, 'width': .2, 'height': .2}})
        with self.assertRaises(ValidationError):
            validate_feedback(value)
        value['schemaVersion'] = '0.2.0'
        validate_feedback(value)

    def test_visual_only_task_needs_schema_0_2_0(self) -> None:
        for kind in ('bug', 'visual-regression', 'task', 'question'):
            value = copy.deepcopy(EXAMPLE)
            value['task'].update(kind=kind, description='', expectedResult='', actualResult='',
                                 reproductionSteps=[])
            with self.assertRaises(ValidationError, msg=kind):
                validate_feedback(value)
            value['schemaVersion'] = '0.2.0'
            validate_feedback(value)

    def test_schema_0_1_0_keeps_its_prose_minimums(self) -> None:
        value = copy.deepcopy(EXAMPLE)
        value['task']['kind'] = 'question'
        value['task'].update(expectedResult='', actualResult='')
        validate_feedback(value)
        value['task']['description'] = ''
        with self.assertRaises(ValidationError):
            validate_feedback(value)
        value = copy.deepcopy(EXAMPLE)
        value['task'].update(kind='bug', actualResult='')
        with self.assertRaises(ValidationError):
            validate_feedback(value)

    def test_task_title_is_required_in_every_version(self) -> None:
        value = drawing_example()
        value['task']['title'] = ''
        with self.assertRaises(ValidationError):
            validate_feedback(value)

    def test_new_shape_kinds_validate_like_their_base_geometry(self) -> None:
        value = drawing_example()
        style = {'stroke': '#112233', 'fill': '#ffeedd', 'strokeWidthPx': 2}
        value['annotations'] = [
            {'id': 'e', 'kind': 'ellipse', 'geometry': {'x': .1, 'y': .1, 'width': .3, 'height': .2}, 'style': style},
            {'id': 'd', 'kind': 'diamond', 'geometry': {'x': .5, 'y': .5, 'width': .5, 'height': .5}},
            {'id': 'l', 'kind': 'line', 'geometry': {'start': {'x': 0, 'y': 0}, 'end': {'x': 1, 'y': 1}}},
        ]
        validate_feedback(value)
        for index, geometry in [(0, {'x': .9, 'y': .1, 'width': .3, 'height': .2}),
                                (1, {'x': .1, 'y': .9, 'width': .2, 'height': .3})]:
            broken = copy.deepcopy(value)
            broken['annotations'][index]['geometry'] = geometry
            with self.assertRaises(ValueError):
                validate_feedback(broken)
        broken = copy.deepcopy(value)
        broken['annotations'][2]['geometry'] = {'start': {'x': 0, 'y': 0}}
        with self.assertRaises(ValidationError):
            validate_feedback(broken)
        broken = copy.deepcopy(value)
        broken['annotations'][0]['style'] = {'fill': 'red'}
        with self.assertRaises(ValidationError):
            validate_feedback(broken)

    @staticmethod
    def advanced(annotations: list[dict[str, Any]], version: str = '0.3.0') -> dict[str, Any]:
        value = drawing_example()
        value['schemaVersion'] = version
        value['annotations'] = annotations
        return value

    def test_schema_0_3_0_accepts_every_new_kind_and_style(self) -> None:
        rect = {'x': .1, 'y': .1, 'width': .3, 'height': .2}
        segment = {'start': {'x': 0, 'y': 0}, 'end': {'x': 1, 'y': 1}}
        value = self.advanced([
            {'id': 'h', 'kind': 'highlight', 'geometry': rect, 'style': {'stroke': '#f2a900', 'opacity': .35}},
            {'id': 's', 'kind': 'spotlight', 'geometry': rect},
            {'id': 'b', 'kind': 'blur', 'geometry': rect, 'style': {'effectSizePx': 12}},
            {'id': 'p', 'kind': 'pixelate', 'geometry': rect, 'style': {'effectSizePx': 64}},
            {'id': 'a', 'kind': 'arrow', 'style': {'arrowStyle': 'curved', 'stroke': '#e5362b'},
             'geometry': {**segment, 'control': {'x': .5, 'y': 0}}},
            {'id': 'a2', 'kind': 'arrow', 'geometry': segment, 'style': {'arrowStyle': 'double'}},
            {'id': 't', 'kind': 'text', 'geometry': {'position': {'x': .5, 'y': .5}, 'text': 'x'},
             'style': {'textStyle': 'mono', 'fontSizePx': 20}},
            {'id': 'f', 'kind': 'freehand', 'geometry': {'points': [{'x': 0, 'y': 0}, {'x': 1, 'y': 1}]},
             'style': {'smoothing': True}},
            {'id': 'r', 'kind': 'redact', 'geometry': rect, 'mode': 'solid'},
        ])
        validate_feedback(value)
        for index, geometry in [(0, {'x': .9, 'y': .1, 'width': .3, 'height': .2}),
                                (3, {'x': .1, 'y': .9, 'width': .2, 'height': .3})]:
            broken = copy.deepcopy(value)
            broken['annotations'][index]['geometry'] = geometry
            with self.assertRaises(ValueError):
                validate_feedback(broken)

    def test_schema_0_1_0_and_0_2_0_reject_every_0_3_0_feature(self) -> None:
        rect = {'x': .1, 'y': .1, 'width': .3, 'height': .2}
        segment = {'start': {'x': 0, 'y': 0}, 'end': {'x': 1, 'y': 1}}
        features = [
            {'id': 'k', 'kind': kind, 'geometry': rect} for kind in ('highlight', 'spotlight', 'blur', 'pixelate')
        ] + [
            {'id': 'c', 'kind': 'arrow', 'geometry': {**segment, 'control': {'x': .5, 'y': 0}}},
            {'id': 'as', 'kind': 'arrow', 'geometry': segment, 'style': {'arrowStyle': 'open'}},
            {'id': 'ts', 'kind': 'text', 'geometry': {'position': {'x': 0, 'y': 0}, 'text': 'x'}, 'style': {'textStyle': 'bold'}},
            {'id': 'sm', 'kind': 'freehand', 'geometry': {'points': [{'x': 0, 'y': 0}, {'x': 1, 'y': 1}]}, 'style': {'smoothing': False}},
        ]
        for annotation in features:
            for version in ('0.1.0', '0.2.0'):
                value = copy.deepcopy(EXAMPLE)
                value['schemaVersion'] = version
                value['annotations'] = [annotation]
                with self.assertRaises(ValidationError, msg=f"{version} {annotation['id']}"):
                    validate_feedback(value)
            # The same data is valid under 0.3.0.
            validate_feedback(self.advanced([annotation]))

    def test_0_3_0_styles_are_bounded_and_bound_to_their_kind(self) -> None:
        rect = {'x': .1, 'y': .1, 'width': .3, 'height': .2}
        segment = {'start': {'x': 0, 'y': 0}, 'end': {'x': 1, 'y': 1}}
        points = {'points': [{'x': 0, 'y': 0}, {'x': 1, 'y': 1}]}
        text = {'position': {'x': 0, 'y': 0}, 'text': 'x'}
        invalid = [
            ('highlight', rect, {'opacity': .09}), ('highlight', rect, {'opacity': .81}),
            ('blur', rect, {'effectSizePx': 1}), ('blur', rect, {'effectSizePx': 65}),
            ('pixelate', rect, {'effectSizePx': 8.5}),
            ('arrow', segment, {'arrowStyle': 'wavy'}), ('text', text, {'textStyle': 'neon'}),
            ('freehand', points, {'smoothing': 'yes'}),
            # Each new style belongs to exactly one kind.
            ('rectangle', rect, {'opacity': .5}), ('spotlight', rect, {'opacity': .5}),
            ('highlight', rect, {'effectSizePx': 8}), ('highlight', rect, {'arrowStyle': 'open'}),
            ('line', segment, {'arrowStyle': 'open'}), ('arrow', segment, {'textStyle': 'bold'}),
            ('text', text, {'smoothing': True}), ('ellipse', rect, {'smoothing': True}),
        ]
        for kind, geometry, style in invalid:
            with self.assertRaises(ValidationError, msg=f'{kind} {style}'):
                validate_feedback(self.advanced([{'id': 'a', 'kind': kind, 'geometry': geometry, 'style': style}]))

    def test_control_point_is_arrow_only_and_normalized(self) -> None:
        segment = {'start': {'x': 0, 'y': 0}, 'end': {'x': 1, 'y': 1}}
        for kind, control in [('line', {'x': .5, 'y': .5}), ('arrow', {'x': 1.1, 'y': .5}), ('arrow', {'x': .5})]:
            geometry = {**segment, 'control': control}
            with self.assertRaises(ValidationError, msg=f'{kind} {control}'):
                validate_feedback(self.advanced([{'id': 'a', 'kind': kind, 'geometry': geometry}]))

    def test_blur_and_pixelate_are_not_redaction(self) -> None:
        rect = {'x': .1, 'y': .1, 'width': .3, 'height': .2}
        for kind in ('blur', 'pixelate', 'highlight', 'spotlight'):
            with self.assertRaises(ValidationError, msg=kind):
                validate_feedback(self.advanced([{'id': 'a', 'kind': kind, 'geometry': rect, 'mode': 'solid'}]))

    def test_coordinate_center_with_letterboxing(self) -> None:
        result = normalized_pointer(520, 350, 40, 80, 960, 540)
        self.assertEqual(result, (.5, .5))
        self.assertEqual(viewport_css(*result, EXAMPLE['capture']), (640, 360))

    def test_coordinate_round_trips(self) -> None:
        rng = random.Random(20261024)
        for _ in range(500):
            x, y = rng.random(), rng.random()
            left, top = rng.uniform(-500, 500), rng.uniform(-500, 500)
            width, height = rng.uniform(100, 4000), rng.uniform(100, 4000)
            actual = normalized_pointer(left+x*width, top+y*height, left, top, width, height)
            self.assertAlmostEqual(x, actual[0], places=12)
            self.assertAlmostEqual(y, actual[1], places=12)

    def test_invalid_coordinate_inputs_are_rejected(self) -> None:
        for args in [(0, 0, 0, 0, 0, 10), (0, 0, 0, 0, 10, -1),
                     (float('nan'), 0, 0, 0, 10, 10), (-1, 0, 0, 0, 10, 10)]:
            with self.assertRaises(ValueError):
                normalized_pointer(*args)

    def test_all_local_schema_references_resolve(self) -> None:
        for node, source in [(SCHEMA, ROOT/'schemas/feedback.schema.json'),
                             (OPENAPI, ROOT/'contracts/openapi.yaml')]:
            for reference in walk_refs(node):
                validate_local_reference(reference, source)

    def test_openapi_structural_consistency(self) -> None:
        self.assertEqual(OPENAPI['openapi'], '3.1.0')
        ids: set[str] = set()
        for path, methods in OPENAPI['paths'].items():
            expected_params = set(re.findall(r'\{([^}]+)\}', path))
            for operation in methods.values():
                operation_id = operation['operationId']
                self.assertNotIn(operation_id, ids)
                ids.add(operation_id)
                path_params = {p['name'] for p in operation.get('parameters', []) if p['in'] == 'path'}
                self.assertEqual(expected_params, path_params)
                self.assertTrue(any(code.startswith('2') for code in operation['responses']))
        for component in OPENAPI['components']['schemas'].values():
            Draft202012Validator.check_schema(component)

    def test_fontconfig_xml_parses(self) -> None:
        tree = ET.parse(ROOT/'docker/browser/fontconfig.reference.conf')
        self.assertEqual(tree.getroot().tag, 'fontconfig')

    @unittest.skipUnless(shutil.which('node'), 'Node.js is unavailable')
    def test_fixture_javascript_syntax(self) -> None:
        html = (ROOT/'tests/fixtures/cjk-emoji.html').read_text(encoding='utf-8')
        script = re.search(r'<script>(.*?)</script>', html, re.S)
        self.assertIsNotNone(script)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)/'fixture.js'
            path.write_text(script.group(1), encoding='utf-8')
            result = subprocess.run(['node', '--check', str(path)], capture_output=True, text=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.parse_args()
    result = unittest.TextTestRunner(verbosity=2).run(unittest.defaultTestLoader.loadTestsFromTestCase(ContractTests))
    return 0 if result.wasSuccessful() else 1


if __name__ == '__main__':
    raise SystemExit(main())
