package gateway

import "github.com/chenyme/grok2api/backend/internal/infra/provider"

func imageParameters(model, prompt string, o provider.ImageRequestOptions) map[string]any {
	return map[string]any{
		"model": model, "prompt": prompt, "n": o.Count, "size": o.Size, "aspect_ratio": o.AspectRatio,
		"resolution": o.Resolution, "quality": o.Quality, "response_format": o.ResponseFormat,
		"output_format": o.OutputFormat, "background": o.Background, "stream": o.Streaming, "partial_images": o.PartialImages,
		"input_images": o.InputImages, "has_mask": o.HasMask,
	}
}

func generatedImageParameters(input ImageGenerationInput) map[string]any {
	return imageParameters(input.PublicModel, input.Prompt, provider.ImageRequestOptions{Count: input.Count, Size: input.Size, AspectRatio: input.AspectRatio, Resolution: input.Resolution, Quality: input.Quality, ResponseFormat: input.ResponseFormat, OutputFormat: input.OutputFormat, Background: input.Background, Streaming: input.Streaming, PartialImages: input.PartialImages})
}

func editedImageParameters(input ImageEditInput) map[string]any {
	values := imageParameters(input.PublicModel, input.Prompt, provider.ImageRequestOptions{Edit: true, Count: input.Count, InputImages: len(input.ImageURLs), HasMask: input.MaskURL != "", Size: input.Size, AspectRatio: input.AspectRatio, Resolution: input.Resolution, Quality: input.Quality, ResponseFormat: input.ResponseFormat, OutputFormat: input.OutputFormat, Background: input.Background, Streaming: input.Streaming, PartialImages: input.PartialImages})
	values["images"], values["mask"] = input.ImageURLs, input.MaskURL
	return values
}

func videoParameters(input VideoInput) map[string]any {
	return map[string]any{"model": input.PublicModel, "prompt": input.Prompt, "operation": input.Operation, "duration": input.Duration, "aspect_ratio": input.AspectRatio, "resolution": input.Resolution, "image": input.ImageURL, "last_frame": input.LastFrameURL, "reference_images": input.ReferenceURLs, "reference_videos": input.ReferenceVideoURLs, "reference_audios": input.ReferenceAudios, "video": input.VideoURL}
}

func imageJobParameters(input ImageJobInput) map[string]any {
	return map[string]any{"model": input.PublicModel, "prompt": input.Prompt, "n": 1, "size": input.Size, "quality": input.Quality, "output_format": input.OutputFormat, "background": input.Background, "images": input.ImageURLs, "mask": input.MaskURL}
}
