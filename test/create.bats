#!/usr/bin/env bats -t
# umoci: Umoci Modifies Open Containers' Images
# Copyright (C) 2016 SUSE LLC.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

load helpers

function setup() {
	setup_image
}

function teardown() {
	teardown_image
}

@test "umoci create --image [empty]" {
	# Setup up $NEWIMAGE.
	NEWIMAGE=$(mktemp -d --tmpdir="$BATS_TMPDIR" image-XXXXX)
	rm -rf "$NEWIMAGE"

	# Create a new image with no tags.
	umoci create --image "$NEWIMAGE"
	[ "$status" -eq 0 ]

	# Make sure that there's no references or blobs.
	sane_run find "$NEWIMAGE/blobs" -type f
	[ "$status" -eq 0 ]
	[ "${#lines[@]}" -eq 0 ]
	sane_run find "$NEWIMAGE/refs" -type f
	[ "$status" -eq 0 ]
	[ "${#lines[@]}" -eq 0 ]

	# Make sure that the required files exist.
	[ -f "$NEWIMAGE/oci-layout" ]
	[ -d "$NEWIMAGE/blobs" ]
	[ -d "$NEWIMAGE/blobs/sha256" ]
	[ -d "$NEWIMAGE/refs" ]
}

@test "umoci create --image --tag" {
	BUNDLE="$(setup_bundle)"

	# Setup up $NEWIMAGE.
	export NEWIMAGE=$(mktemp -d --tmpdir="$BATS_TMPDIR" image-XXXXX)
	rm -rf "$NEWIMAGE"

	# Create a new image with another tag.
	umoci create --image "$NEWIMAGE" --tag "latest"
	[ "$status" -eq 0 ]

	# Modify the config.
	umoci config --image "$NEWIMAGE" --from "latest" --tag "latest" --config.user "1234:1332"
	[ "$status" -eq 0 ]

	# Unpack the image.
	umoci unpack --image "$NEWIMAGE" --from "latest" --bundle "$BUNDLE"
	[ "$status" -eq 0 ]

	# Make sure that the rootfs is empty.
	sane_run find "$BUNDLE/rootfs"
	[ "$status" -eq 0 ]
	[ "${#lines[@]}" -eq 1 ]

	# Make sure that the config applied.
	sane_run jq -SM '.process.user.uid' "$BUNDLE/config.json"
	[ "$status" -eq 0 ]
	[ "$output" -eq 1234 ]

	# Make sure numeric config was actually set.
	sane_run jq -SM '.process.user.gid' "$BUNDLE/config.json"
	[ "$status" -eq 0 ]
	[ "$output" -eq 1332 ]

	# Make sure additionalGids were not set.
	sane_run jq -SMr '.process.user.additionalGids' "$BUNDLE/config.json"
	[ "$status" -eq 0 ]
	[[ "$output" == "null" ]]
}
