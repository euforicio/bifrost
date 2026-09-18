package cursor

import (
	"context"

	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

type unsupportedProvider struct{ providerKey schemas.ModelProvider }

func (p unsupportedProvider) unsupported(requestType schemas.RequestType) *schemas.BifrostError {
	return providerUtils.NewUnsupportedOperationError(requestType, p.providerKey)
}

func (p unsupportedProvider) GetProviderKey() schemas.ModelProvider { return p.providerKey }
func (p unsupportedProvider) ListModels(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostListModelsRequest) (*schemas.BifrostListModelsResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ListModelsRequest)
}
func (p unsupportedProvider) TextCompletion(*schemas.BifrostContext, schemas.Key, *schemas.BifrostTextCompletionRequest) (*schemas.BifrostTextCompletionResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.TextCompletionRequest)
}
func (p unsupportedProvider) TextCompletionStream(*schemas.BifrostContext, schemas.PostHookRunner, func(context.Context), schemas.Key, *schemas.BifrostTextCompletionRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.TextCompletionStreamRequest)
}
func (p unsupportedProvider) ChatCompletion(*schemas.BifrostContext, schemas.Key, *schemas.BifrostChatRequest) (*schemas.BifrostChatResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ChatCompletionRequest)
}
func (p unsupportedProvider) ChatCompletionStream(*schemas.BifrostContext, schemas.PostHookRunner, func(context.Context), schemas.Key, *schemas.BifrostChatRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ChatCompletionStreamRequest)
}
func (p unsupportedProvider) Responses(*schemas.BifrostContext, schemas.Key, *schemas.BifrostResponsesRequest) (*schemas.BifrostResponsesResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ResponsesRequest)
}
func (p unsupportedProvider) ResponsesStream(*schemas.BifrostContext, schemas.PostHookRunner, func(context.Context), schemas.Key, *schemas.BifrostResponsesRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ResponsesStreamRequest)
}
func (p unsupportedProvider) CountTokens(*schemas.BifrostContext, schemas.Key, *schemas.BifrostResponsesRequest) (*schemas.BifrostCountTokensResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.CountTokensRequest)
}
func (p unsupportedProvider) Compaction(*schemas.BifrostContext, schemas.Key, *schemas.BifrostCompactionRequest) (*schemas.BifrostCompactionResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.CompactionRequest)
}
func (p unsupportedProvider) Embedding(*schemas.BifrostContext, schemas.Key, *schemas.BifrostEmbeddingRequest) (*schemas.BifrostEmbeddingResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.EmbeddingRequest)
}
func (p unsupportedProvider) Rerank(*schemas.BifrostContext, schemas.Key, *schemas.BifrostRerankRequest) (*schemas.BifrostRerankResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.RerankRequest)
}
func (p unsupportedProvider) OCR(*schemas.BifrostContext, schemas.Key, *schemas.BifrostOCRRequest) (*schemas.BifrostOCRResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.OCRRequest)
}
func (p unsupportedProvider) Speech(*schemas.BifrostContext, schemas.Key, *schemas.BifrostSpeechRequest) (*schemas.BifrostSpeechResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.SpeechRequest)
}
func (p unsupportedProvider) SpeechStream(*schemas.BifrostContext, schemas.PostHookRunner, func(context.Context), schemas.Key, *schemas.BifrostSpeechRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.SpeechStreamRequest)
}
func (p unsupportedProvider) Transcription(*schemas.BifrostContext, schemas.Key, *schemas.BifrostTranscriptionRequest) (*schemas.BifrostTranscriptionResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.TranscriptionRequest)
}
func (p unsupportedProvider) TranscriptionStream(*schemas.BifrostContext, schemas.PostHookRunner, func(context.Context), schemas.Key, *schemas.BifrostTranscriptionRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.TranscriptionStreamRequest)
}
func (p unsupportedProvider) ImageGeneration(*schemas.BifrostContext, schemas.Key, *schemas.BifrostImageGenerationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ImageGenerationRequest)
}
func (p unsupportedProvider) ImageGenerationStream(*schemas.BifrostContext, schemas.PostHookRunner, func(context.Context), schemas.Key, *schemas.BifrostImageGenerationRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ImageGenerationStreamRequest)
}
func (p unsupportedProvider) ImageEdit(*schemas.BifrostContext, schemas.Key, *schemas.BifrostImageEditRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ImageEditRequest)
}
func (p unsupportedProvider) ImageEditStream(*schemas.BifrostContext, schemas.PostHookRunner, func(context.Context), schemas.Key, *schemas.BifrostImageEditRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ImageEditStreamRequest)
}
func (p unsupportedProvider) ImageVariation(*schemas.BifrostContext, schemas.Key, *schemas.BifrostImageVariationRequest) (*schemas.BifrostImageGenerationResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ImageVariationRequest)
}
func (p unsupportedProvider) VideoGeneration(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoGenerationRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.VideoGenerationRequest)
}
func (p unsupportedProvider) VideoEdit(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoEditRequest) (*schemas.BifrostVideoEditResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.VideoEditRequest)
}
func (p unsupportedProvider) VideoRetrieve(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoRetrieveRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.VideoRetrieveRequest)
}
func (p unsupportedProvider) VideoDownload(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoDownloadRequest) (*schemas.BifrostVideoDownloadResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.VideoDownloadRequest)
}
func (p unsupportedProvider) VideoDelete(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoDeleteRequest) (*schemas.BifrostVideoDeleteResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.VideoDeleteRequest)
}
func (p unsupportedProvider) VideoList(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoListRequest) (*schemas.BifrostVideoListResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.VideoListRequest)
}
func (p unsupportedProvider) VideoRemix(*schemas.BifrostContext, schemas.Key, *schemas.BifrostVideoRemixRequest) (*schemas.BifrostVideoGenerationResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.VideoRemixRequest)
}
func (p unsupportedProvider) BatchCreate(*schemas.BifrostContext, schemas.Key, *schemas.BifrostBatchCreateRequest) (*schemas.BifrostBatchCreateResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.BatchCreateRequest)
}
func (p unsupportedProvider) BatchList(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostBatchListRequest) (*schemas.BifrostBatchListResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.BatchListRequest)
}
func (p unsupportedProvider) BatchRetrieve(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostBatchRetrieveRequest) (*schemas.BifrostBatchRetrieveResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.BatchRetrieveRequest)
}
func (p unsupportedProvider) BatchCancel(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostBatchCancelRequest) (*schemas.BifrostBatchCancelResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.BatchCancelRequest)
}
func (p unsupportedProvider) BatchDelete(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostBatchDeleteRequest) (*schemas.BifrostBatchDeleteResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.BatchDeleteRequest)
}
func (p unsupportedProvider) BatchResults(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostBatchResultsRequest) (*schemas.BifrostBatchResultsResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.BatchResultsRequest)
}
func (p unsupportedProvider) FileUpload(*schemas.BifrostContext, schemas.Key, *schemas.BifrostFileUploadRequest) (*schemas.BifrostFileUploadResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.FileUploadRequest)
}
func (p unsupportedProvider) FileList(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostFileListRequest) (*schemas.BifrostFileListResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.FileListRequest)
}
func (p unsupportedProvider) FileRetrieve(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostFileRetrieveRequest) (*schemas.BifrostFileRetrieveResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.FileRetrieveRequest)
}
func (p unsupportedProvider) FileDelete(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostFileDeleteRequest) (*schemas.BifrostFileDeleteResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.FileDeleteRequest)
}
func (p unsupportedProvider) FileContent(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostFileContentRequest) (*schemas.BifrostFileContentResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.FileContentRequest)
}
func (p unsupportedProvider) CachedContentCreate(*schemas.BifrostContext, schemas.Key, *schemas.BifrostCachedContentCreateRequest) (*schemas.BifrostCachedContentCreateResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.CachedContentCreateRequest)
}
func (p unsupportedProvider) CachedContentList(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostCachedContentListRequest) (*schemas.BifrostCachedContentListResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.CachedContentListRequest)
}
func (p unsupportedProvider) CachedContentRetrieve(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostCachedContentRetrieveRequest) (*schemas.BifrostCachedContentRetrieveResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.CachedContentRetrieveRequest)
}
func (p unsupportedProvider) CachedContentUpdate(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostCachedContentUpdateRequest) (*schemas.BifrostCachedContentUpdateResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.CachedContentUpdateRequest)
}
func (p unsupportedProvider) CachedContentDelete(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostCachedContentDeleteRequest) (*schemas.BifrostCachedContentDeleteResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.CachedContentDeleteRequest)
}
func (p unsupportedProvider) ContainerCreate(*schemas.BifrostContext, schemas.Key, *schemas.BifrostContainerCreateRequest) (*schemas.BifrostContainerCreateResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ContainerCreateRequest)
}
func (p unsupportedProvider) ContainerList(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostContainerListRequest) (*schemas.BifrostContainerListResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ContainerListRequest)
}
func (p unsupportedProvider) ContainerRetrieve(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostContainerRetrieveRequest) (*schemas.BifrostContainerRetrieveResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ContainerRetrieveRequest)
}
func (p unsupportedProvider) ContainerDelete(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostContainerDeleteRequest) (*schemas.BifrostContainerDeleteResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ContainerDeleteRequest)
}
func (p unsupportedProvider) ContainerFileCreate(*schemas.BifrostContext, schemas.Key, *schemas.BifrostContainerFileCreateRequest) (*schemas.BifrostContainerFileCreateResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ContainerFileCreateRequest)
}
func (p unsupportedProvider) ContainerFileList(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostContainerFileListRequest) (*schemas.BifrostContainerFileListResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ContainerFileListRequest)
}
func (p unsupportedProvider) ContainerFileRetrieve(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostContainerFileRetrieveRequest) (*schemas.BifrostContainerFileRetrieveResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ContainerFileRetrieveRequest)
}
func (p unsupportedProvider) ContainerFileContent(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostContainerFileContentRequest) (*schemas.BifrostContainerFileContentResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ContainerFileContentRequest)
}
func (p unsupportedProvider) ContainerFileDelete(*schemas.BifrostContext, []schemas.Key, *schemas.BifrostContainerFileDeleteRequest) (*schemas.BifrostContainerFileDeleteResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.ContainerFileDeleteRequest)
}
func (p unsupportedProvider) Passthrough(*schemas.BifrostContext, schemas.Key, *schemas.BifrostPassthroughRequest) (*schemas.BifrostPassthroughResponse, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.PassthroughRequest)
}
func (p unsupportedProvider) PassthroughStream(*schemas.BifrostContext, schemas.PostHookRunner, func(context.Context), schemas.Key, *schemas.BifrostPassthroughRequest) (chan *schemas.BifrostStreamChunk, *schemas.BifrostError) {
	return nil, p.unsupported(schemas.PassthroughStreamRequest)
}
