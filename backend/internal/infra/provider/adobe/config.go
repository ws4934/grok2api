package adobe

import "time"

// Firefly 公网 Web 客户端身份。同一个值既是用 Cookie 续期的 IMS OAuth
// client_id，也是每次生成请求携带的 x-api-key。
const (
	defaultAPIKey    = "clio-playground-web"
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"

	refreshScope = "AdobeID,firefly_api,openid,pps.read,pps.write,additional_info.projectedProductContext,additional_info.ownerOrg,uds_read,uds_write,ab.manage,read_organizations,additional_info.roles,account_cluster.read,creative_production,profile"
)

// 这两个固定端点以 var 声明，便于测试覆盖。
var (
	refreshURL = "https://adobeid-na1.services.adobe.com/ims/check/v6/token?jslVersion=v2-v0.48.0-1-g1e322cb"
	creditsURL = "https://firefly.adobe.io/v1/credits/balance"
)

var profileURLs = []string{
	"https://ims-na1.adobelogin.com/ims/profile/v1",
	"https://adobeid-na1.services.adobe.com/ims/profile/v1",
}

var endpoints = struct {
	imageSubmit string
	videoSubmit string
	imageUpload string
}{
	imageSubmit: "https://firefly-3p.ff.adobe.io/v2/3p-images/generate-async",
	videoSubmit: "https://firefly-3p.ff.adobe.io/v2/3p-videos/generate-async",
	imageUpload: "https://firefly-3p.ff.adobe.io/v2/storage/image",
}

// Config 保存适配器每次请求读取的 Adobe 运行时设置。
type Config struct {
	// RequestTimeout 限制单次上游调用；0 表示 60 秒。
	RequestTimeout time.Duration
	// SherlockMintAPI 是 mint sidecar 的基址（GET /mint）；留空则无法提交。
	SherlockMintAPI string
}

func (c Config) requestTimeout() time.Duration {
	if c.RequestTimeout > 0 {
		return c.RequestTimeout
	}
	return 60 * time.Second
}
