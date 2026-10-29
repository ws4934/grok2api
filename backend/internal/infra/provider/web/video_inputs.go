package web

import (
	"context"
	"fmt"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// The official Imagine frontend defaults max_r2v_attachments to seven.
const maxWebVideoReferences = 7

// prepareVideoImages shares the existing Imagine direct upload and SSRF-safe
// image loader. Only fileMetadataId belongs in mediaGenInput.inputAssets.
func (a *Adapter) prepareVideoImages(ctx context.Context, cfg Config, lease *egress.Lease, token, firstFrame string, references []string) (string, []string, error) {
	if strings.TrimSpace(firstFrame) == "" && len(references) == 0 {
		return "", nil, nil
	}
	release, err := provider.HoldInputs(ctx)
	if err != nil {
		return "", nil, err
	}
	defer release()
	upload := func(source string) (string, error) {
		var image provider.ImageInput
		var err error
		if provider.IsInputReference(source) {
			image.Data, image.MIMEType, err = provider.ReadInput(ctx, source)
			if err == nil {
				if int64(len(image.Data)) > cfg.MaxInputImageBytes {
					return "", fmt.Errorf("%w: 图片超过 %d MiB", provider.ErrVideoParameterInvalid, cfg.MaxInputImageBytes>>20)
				}
				image.MIMEType, err = validatedImageMIME(image.Data, image.MIMEType)
				image.Filename = "image" + imageExtension(image.MIMEType)
			}
		} else {
			image, err = a.loadChatImage(ctx, lease, strings.TrimSpace(source), cfg.MaxInputImageBytes)
		}
		if err != nil {
			return "", err
		}
		file, err := a.uploadFileV2Direct(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine", imagineSelfUploadSource, "video_input_upload")
		if err != nil {
			return "", err
		}
		if file.MetadataID == "" {
			return "", fmt.Errorf("上传图片成功但上游未返回 fileMetadataId")
		}
		return file.MetadataID, nil
	}
	firstAsset := ""
	if strings.TrimSpace(firstFrame) != "" {
		if firstAsset, err = upload(firstFrame); err != nil {
			return "", nil, err
		}
	}
	assets := make([]string, 0, len(references))
	for _, reference := range references {
		asset, err := upload(reference)
		if err != nil {
			return "", nil, err
		}
		assets = append(assets, asset)
	}
	return firstAsset, assets, nil
}
