/*
 * umoci: Umoci Modifies Open Containers' Images
 * Copyright (C) 2016 SUSE LLC.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/cyphar/umoci/image/cas"
	"github.com/cyphar/umoci/image/generator"
	"github.com/cyphar/umoci/image/layer"
	"github.com/cyphar/umoci/pkg/idtools"
	"github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/urfave/cli"
	"github.com/vbatts/go-mtree"
	"golang.org/x/net/context"
)

var repackCommand = cli.Command{
	Name:  "repack",
	Usage: "repacks an OCI runtime bundle into a reference",
	ArgsUsage: `--image <image-path> --from <reference> --bundle <bundle-path>

Where "<image-path>" is the path to the OCI image, "<reference>" is the name of
the reference descriptor which was used to generate the original runtime bundle
and "<bundle-path>" is the destination to repack the image to.

It should be noted that this is not the same as oci-create-layer because it
uses go-mtree to create diff layers from runtime bundles unpacked with
umoci-unpack(1). In addition, it modifies the image so that all of the relevant
manifest and configuration information uses the new diff atop the old manifest.`,

	Flags: []cli.Flag{
		// FIXME: This really should be a global option.
		cli.StringFlag{
			Name:  "image",
			Usage: "path to OCI image bundle",
		},
		cli.StringFlag{
			Name:  "from",
			Usage: "reference descriptor name to repack",
		},
		cli.StringFlag{
			Name:  "bundle",
			Usage: "destination bundle path",
		},
		cli.StringFlag{
			Name:  "tag",
			Usage: "tag name for repacked image",
		},
		cli.StringSliceFlag{
			Name:  "uid-map",
			Usage: "specifies a uid mapping to use when repacking",
		},
		cli.StringSliceFlag{
			Name:  "gid-map",
			Usage: "specifies a gid mapping to use when repacking",
		},
		cli.BoolFlag{
			Name:  "rootless",
			Usage: "enable rootless unpacking support",
		},
	},

	Action: repack,
}

func repack(ctx *cli.Context) error {
	// FIXME: Is there a nicer way of dealing with mandatory arguments?
	imagePath := ctx.String("image")
	if imagePath == "" {
		return fmt.Errorf("image path cannot be empty")
	}
	bundlePath := ctx.String("bundle")
	if bundlePath == "" {
		return fmt.Errorf("bundle path cannot be empty")
	}
	fromName := ctx.String("from")
	if fromName == "" {
		return fmt.Errorf("reference name cannot be empty")
	}

	// Parse map options.
	mapOptions := layer.MapOptions{
		Rootless: ctx.Bool("rootless"),
	}
	// We need to set mappings if we're in rootless mode.
	if mapOptions.Rootless {
		if !ctx.IsSet("uid-map") {
			ctx.Set("uid-map", fmt.Sprintf("%d:0:1", os.Geteuid()))
			logrus.WithFields(logrus.Fields{
				"map.uid": ctx.StringSlice("uid-map"),
			}).Info("setting default rootless --uid-map option")
		}
		if !ctx.IsSet("gid-map") {
			ctx.Set("gid-map", fmt.Sprintf("%d:0:1", os.Getegid()))
			logrus.WithFields(logrus.Fields{
				"map.gid": ctx.StringSlice("gid-map"),
			}).Info("setting default rootless --gid-map option")
		}
	}
	for _, uidmap := range ctx.StringSlice("uid-map") {
		idMap, err := idtools.ParseMapping(uidmap)
		if err != nil {
			return fmt.Errorf("failure parsing --uid-map %s: %s", uidmap, err)
		}
		mapOptions.UIDMappings = append(mapOptions.UIDMappings, idMap)
	}
	for _, gidmap := range ctx.StringSlice("gid-map") {
		idMap, err := idtools.ParseMapping(gidmap)
		if err != nil {
			return fmt.Errorf("failure parsing --gid-map %s: %s", gidmap, err)
		}
		mapOptions.GIDMappings = append(mapOptions.GIDMappings, idMap)
	}
	logrus.WithFields(logrus.Fields{
		"map.uid": mapOptions.UIDMappings,
		"map.gid": mapOptions.GIDMappings,
	}).Infof("parsed mappings")

	// Get a reference to the CAS.
	engine, err := cas.Open(imagePath)
	if err != nil {
		return err
	}
	defer engine.Close()

	fromDescriptor, err := engine.GetReference(context.TODO(), fromName)
	if err != nil {
		return err
	}

	// FIXME: Implement support for manifest lists.
	if fromDescriptor.MediaType != v1.MediaTypeImageManifest {
		return fmt.Errorf("--from descriptor does not point to v1.MediaTypeImageManifest: not implemented: %s", fromDescriptor.MediaType)
	}

	mtreeName := strings.Replace(fromDescriptor.Digest, "sha256:", "sha256_", 1)
	mtreePath := filepath.Join(bundlePath, mtreeName+".mtree")
	fullRootfsPath := filepath.Join(bundlePath, layer.RootfsName)

	logrus.WithFields(logrus.Fields{
		"image":  imagePath,
		"bundle": bundlePath,
		"ref":    fromName,
		"rootfs": layer.RootfsName,
		"mtree":  mtreePath,
	}).Debugf("umoci: repacking OCI image")

	mfh, err := os.Open(mtreePath)
	if err != nil {
		return err
	}
	defer mfh.Close()

	spec, err := mtree.ParseSpec(mfh)
	if err != nil {
		return err
	}

	keywords := spec.UsedKeywords()

	logrus.WithFields(logrus.Fields{
		"keywords": keywords,
	}).Debugf("umoci: parsed mtree spec")

	diffs, err := mtree.Check(fullRootfsPath, spec, keywords, mapOptions.Rootless)
	if err != nil {
		return err
	}

	logrus.WithFields(logrus.Fields{
		"ndiff": len(diffs),
	}).Debugf("umoci: checked mtree spec")

	reader, err := layer.GenerateLayer(fullRootfsPath, diffs, &mapOptions)
	if err != nil {
		return err
	}
	defer reader.Close()

	// XXX: I get the feeling all of this should be moved to a separate package
	//      which abstracts this nicely.

	// We need to store the gzip'd layer (which has a blob digest) but we also
	// need to grab the diffID (which is the digest of the *uncompressed*
	// layer). But because we have a Reader from GenerateLayer() we need to use
	// a goroutine.
	// FIXME: This is all super-ugly.

	diffIDHash := sha256.New()
	hashReader := io.TeeReader(reader, diffIDHash)

	pipeReader, pipeWriter := io.Pipe()
	defer pipeReader.Close()

	gzw := gzip.NewWriter(pipeWriter)
	defer gzw.Close()
	go func() {
		_, err := io.Copy(gzw, hashReader)
		if err != nil {
			logrus.Warnf("failed when copying to gzip: %s", err)
			pipeWriter.CloseWithError(err)
			return
		}
		gzw.Close()
		pipeWriter.Close()
	}()

	layerDigest, layerSize, err := engine.PutBlob(context.TODO(), pipeReader)
	if err != nil {
		return err
	}
	reader.Close()
	// XXX: Should we defer a DeleteBlob?

	layerDiffID := fmt.Sprintf("%s:%x", cas.BlobAlgorithm, diffIDHash.Sum(nil))

	layerDescriptor := &v1.Descriptor{
		// FIXME: This should probably be configurable, so someone can specify
		//        that a layer is not distributable.
		MediaType: v1.MediaTypeImageLayer,
		Digest:    layerDigest,
		Size:      layerSize,
	}

	logrus.WithFields(logrus.Fields{
		"digest": layerDigest,
		"size":   layerSize,
	}).Debugf("umoci: generated new diff layer")

	manifestBlob, err := cas.FromDescriptor(context.TODO(), engine, fromDescriptor)
	if err != nil {
		return err
	}
	defer manifestBlob.Close()

	logrus.WithFields(logrus.Fields{
		"digest": manifestBlob.Digest,
	}).Debugf("umoci: got original manifest")

	manifest, ok := manifestBlob.Data.(*v1.Manifest)
	if !ok {
		// Should never be reached.
		return fmt.Errorf("manifest blob type not implemented: %s", manifestBlob.MediaType)
	}

	// We also need to update the config. Fun.
	configBlob, err := cas.FromDescriptor(context.TODO(), engine, &manifest.Config)
	if err != nil {
		return err
	}
	defer configBlob.Close()

	logrus.WithFields(logrus.Fields{
		"digest": configBlob.Digest,
	}).Debugf("umoci: got original config")

	config, ok := configBlob.Data.(*v1.Image)
	if !ok {
		// Should not be reached.
		return fmt.Errorf("config blob type not implemented: %s", configBlob.MediaType)
	}

	g, err := generator.NewFromImage(*config)
	if err != nil {
		return err
	}

	// Append our new layer to the set of DiffIDs.
	g.AddRootfsDiffID(layerDiffID)

	// Update config and create a new blob for it.
	*config = g.Image()
	newConfigDigest, newConfigSize, err := engine.PutBlobJSON(context.TODO(), config)
	if err != nil {
		return err
	}

	logrus.WithFields(logrus.Fields{
		"digest": newConfigDigest,
		"size":   newConfigSize,
	}).Debugf("umoci: added new config")

	// Update the manifest to include the new layer, and also point at the new
	// config. Then create a new blob for it.
	manifest.Layers = append(manifest.Layers, *layerDescriptor)
	manifest.Config.Digest = newConfigDigest
	manifest.Config.Size = newConfigSize
	newManifestDigest, newManifestSize, err := engine.PutBlobJSON(context.TODO(), manifest)

	logrus.WithFields(logrus.Fields{
		"digest": newManifestDigest,
		"size":   newManifestSize,
	}).Debugf("umoci: added new manifest")

	// Now create a new reference, and either add it to the engine or spew it
	// to stdout.

	newDescriptor := &v1.Descriptor{
		// FIXME: Support manifest lists.
		MediaType: v1.MediaTypeImageManifest,
		Digest:    newManifestDigest,
		Size:      newManifestSize,
	}

	logrus.WithFields(logrus.Fields{
		"mediatype": newDescriptor.MediaType,
		"digest":    newDescriptor.Digest,
		"size":      newDescriptor.Size,
	}).Infof("created new image")

	tagName := ctx.String("tag")
	if tagName == "" {
		return nil
	}

	// We have to clobber the old reference.
	// XXX: Should we output some warning if we actually did remove an old
	//      reference?
	if err := engine.DeleteReference(context.TODO(), tagName); err != nil {
		return err
	}
	if err := engine.PutReference(context.TODO(), tagName, newDescriptor); err != nil {
		return err
	}

	return nil
}
