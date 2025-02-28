package provider

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/Azure/secrets-store-csi-driver-provider-azure/pkg/auth"
	"github.com/Azure/secrets-store-csi-driver-provider-azure/pkg/version"

	kv "github.com/Azure/azure-sdk-for-go/services/keyvault/2016-10-01/keyvault"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/pkg/errors"
	"golang.org/x/crypto/pkcs12"
	"golang.org/x/net/context"
	"gopkg.in/yaml.v2"
	"k8s.io/klog/v2"
)

var (
	ConstructPEMChain  = flag.Bool("construct-pem-chain", false, "explicitly reconstruct the pem chain in the order: SERVER, INTERMEDIATE, ROOT")
	DriverWriteSecrets = flag.Bool("driver-write-secrets", true, "Return secrets in gRPC response to the driver (supported in driver v0.0.21+) instead of writing to filesystem")
)

// Type of Azure Key Vault objects
const (
	// VaultObjectTypeSecret secret vault object type
	VaultObjectTypeSecret string = "secret"
	// VaultObjectTypeKey key vault object type
	VaultObjectTypeKey string = "key"
	// VaultObjectTypeCertificate certificate vault object type
	VaultObjectTypeCertificate string = "cert"

	certTypePem          = "application/x-pem-file"
	certTypePfx          = "application/x-pkcs12"
	certificateType      = "CERTIFICATE"
	objectFormatPEM      = "pem"
	objectFormatPFX      = "pfx"
	objectEncodingHex    = "hex"
	objectEncodingBase64 = "base64"
	objectEncodingUtf8   = "utf-8"

	// pod identity NMI port
	// TODO (aramase) make this configurable during the provider deployment
	podIdentityNMIPort = "2579"
)

// Provider implements the secrets-store-csi-driver provider interface
type Provider struct {
	// the name of the Azure Key Vault instance
	KeyvaultName string
	// the type of azure cloud based on azure go sdk
	AzureCloudEnvironment *azure.Environment
	// the name of the Azure Key Vault objects, since attributes can only be strings
	// this will be mapped to StringArray, which is an array of KeyVaultObject
	Objects []KeyVaultObject
	// AuthConfig is the config parameters for accessing Key Vault
	AuthConfig auth.Config
	// TenantID in AAD
	TenantID string
	// PodName is the pod name
	PodName string
	// PodNamespace is the pod namespace
	PodNamespace string
	// EnvironmentFilepathName captures the name of the environment variable containing the path to the file
	// to be used while populating the Azure Environment.
	EnvironmentFilepathName string
}

// KeyVaultObject holds keyvault object related config
type KeyVaultObject struct {
	// the name of the Azure Key Vault objects
	ObjectName string `json:"objectName" yaml:"objectName"`
	// the filename the object will be written to
	ObjectAlias string `json:"objectAlias" yaml:"objectAlias"`
	// the version of the Azure Key Vault objects
	ObjectVersion string `json:"objectVersion" yaml:"objectVersion"`
	// the type of the Azure Key Vault objects
	ObjectType string `json:"objectType" yaml:"objectType"`
	// the format of the Azure Key Vault objects
	// supported formats are PEM, PFX
	ObjectFormat string `json:"objectFormat" yaml:"objectFormat"`
	// The encoding of the object in KeyVault
	// Supported encodings are Base64, Hex, Utf-8
	ObjectEncoding string `json:"objectEncoding" yaml:"objectEncoding"`
}

// StringArray ...
type StringArray struct {
	Array []string `json:"array" yaml:"array"`
}

// NewProvider creates a new Azure Key Vault Provider.
func NewProvider() (*Provider, error) {
	var p Provider
	return &p, nil
}

// ParseAzureEnvironment returns azure environment by name
func ParseAzureEnvironment(cloudName string) (*azure.Environment, error) {
	var env azure.Environment
	var err error
	if cloudName == "" {
		env = azure.PublicCloud
	} else {
		env, err = azure.EnvironmentFromName(cloudName)
	}
	return &env, err
}

// GetKeyvaultToken retrieves a new service principal token to access keyvault
func (p *Provider) GetKeyvaultToken() (authorizer autorest.Authorizer, err error) {
	kvEndPoint := p.AzureCloudEnvironment.KeyVaultEndpoint
	if '/' == kvEndPoint[len(kvEndPoint)-1] {
		kvEndPoint = kvEndPoint[:len(kvEndPoint)-1]
	}
	servicePrincipalToken, err := p.GetServicePrincipalToken(kvEndPoint)
	if err != nil {
		return nil, err
	}
	authorizer = autorest.NewBearerAuthorizer(servicePrincipalToken)
	return authorizer, nil
}

func (p *Provider) initializeKvClient() (*kv.BaseClient, error) {
	kvClient := kv.New()
	err := kvClient.AddToUserAgent(version.GetUserAgent())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to add user agent to keyvault client")
	}
	token, err := p.GetKeyvaultToken()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get key vault token")
	}

	kvClient.Authorizer = token
	return &kvClient, nil
}

func (p *Provider) getVaultURL(ctx context.Context) (vaultURL *string, err error) {
	klog.V(2).Infof("vaultName: %s", p.KeyvaultName)

	// Key Vault name must be a 3-24 character string
	if len(p.KeyvaultName) < 3 || len(p.KeyvaultName) > 24 {
		return nil, errors.Errorf("Invalid vault name: %q, must be between 3 and 24 chars", p.KeyvaultName)
	}
	// See docs for validation spec: https://docs.microsoft.com/en-us/azure/key-vault/about-keys-secrets-and-certificates#objects-identifiers-and-versioning
	isValid := regexp.MustCompile(`^[-A-Za-z0-9]+$`).MatchString
	if !isValid(p.KeyvaultName) {
		return nil, errors.Errorf("Invalid vault name: %q, must match [-a-zA-Z0-9]{3,24}", p.KeyvaultName)
	}

	vaultDNSSuffixValue := p.AzureCloudEnvironment.KeyVaultDNSSuffix
	vaultURI := "https://" + p.KeyvaultName + "." + vaultDNSSuffixValue + "/"
	return &vaultURI, nil
}

// GetServicePrincipalToken creates a new service principal token based on the configuration
func (p *Provider) GetServicePrincipalToken(resource string) (*adal.ServicePrincipalToken, error) {
	return p.AuthConfig.GetServicePrincipalToken(p.PodName, p.PodNamespace, resource, p.AzureCloudEnvironment.ActiveDirectoryEndpoint, p.TenantID, podIdentityNMIPort)
}

// MountSecretsStoreObjectContent mounts content of the secrets store object to target path
func (p *Provider) MountSecretsStoreObjectContent(ctx context.Context, attrib map[string]string, secrets map[string]string, targetPath string, permission os.FileMode) (map[string][]byte, map[string]string, error) {
	keyvaultName := strings.TrimSpace(attrib["keyvaultName"])
	cloudName := strings.TrimSpace(attrib["cloudName"])
	usePodIdentityStr := strings.TrimSpace(attrib["usePodIdentity"])
	useVMManagedIdentityStr := strings.TrimSpace(attrib["useVMManagedIdentity"])
	userAssignedIdentityID := strings.TrimSpace(attrib["userAssignedIdentityID"])
	tenantID := strings.TrimSpace(attrib["tenantId"])
	cloudEnvFileName := strings.TrimSpace(attrib["cloudEnvFileName"])
	p.PodName = strings.TrimSpace(attrib["csi.storage.k8s.io/pod.name"])
	p.PodNamespace = strings.TrimSpace(attrib["csi.storage.k8s.io/pod.namespace"])

	if keyvaultName == "" {
		return nil, nil, fmt.Errorf("keyvaultName is not set")
	}
	if tenantID == "" {
		return nil, nil, fmt.Errorf("tenantId is not set")
	}
	if len(usePodIdentityStr) == 0 {
		usePodIdentityStr = "false"
	}
	usePodIdentity, err := strconv.ParseBool(usePodIdentityStr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse usePodIdentity flag, error: %w", err)
	}
	if len(useVMManagedIdentityStr) == 0 {
		useVMManagedIdentityStr = "false"
	}
	useVMManagedIdentity, err := strconv.ParseBool(useVMManagedIdentityStr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse useVMManagedIdentity flag, error: %w", err)
	}

	err = setAzureEnvironmentFilePath(cloudEnvFileName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set AZURE_ENVIRONMENT_FILEPATH env to %s, error %w", cloudEnvFileName, err)
	}
	azureCloudEnv, err := ParseAzureEnvironment(cloudName)
	if err != nil {
		return nil, nil, fmt.Errorf("cloudName %s is not valid, error: %w", cloudName, err)
	}

	p.AuthConfig, err = auth.NewConfig(usePodIdentity, useVMManagedIdentity, userAssignedIdentityID, secrets)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create auth config, error: %w", err)
	}

	objectsStrings := attrib["objects"]
	if objectsStrings == "" {
		return nil, nil, fmt.Errorf("objects is not set")
	}
	klog.V(2).InfoS("objects string defined in secret provider class", "objects", objectsStrings, "pod", klog.ObjectRef{Namespace: p.PodNamespace, Name: p.PodName})

	var objects StringArray
	err = yaml.Unmarshal([]byte(objectsStrings), &objects)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to yaml unmarshal objects, error: %w", err)
	}
	klog.V(2).InfoS("unmarshaled objects yaml array", "objectsArray", objects.Array, "pod", klog.ObjectRef{Namespace: p.PodNamespace, Name: p.PodName})

	keyVaultObjects := []KeyVaultObject{}
	for i, object := range objects.Array {
		var keyVaultObject KeyVaultObject
		err = yaml.Unmarshal([]byte(object), &keyVaultObject)
		if err != nil {
			return nil, nil, fmt.Errorf("unmarshal failed for keyVaultObjects at index %d, error: %w", i, err)
		}
		// remove whitespace from all fields in keyVaultObject
		formatKeyVaultObject(&keyVaultObject)
		keyVaultObjects = append(keyVaultObjects, keyVaultObject)
	}

	klog.InfoS("unmarshaled key vault objects", "keyVaultObjects", keyVaultObjects, "count", len(keyVaultObjects), "pod", klog.ObjectRef{Namespace: p.PodNamespace, Name: p.PodName})

	if len(keyVaultObjects) == 0 {
		return nil, nil, fmt.Errorf("objects array is empty")
	}
	p.KeyvaultName = keyvaultName
	p.AzureCloudEnvironment = azureCloudEnv
	p.TenantID = tenantID

	objectVersionMap := make(map[string]string)
	files := make(map[string][]byte)
	for _, keyVaultObject := range keyVaultObjects {
		klog.InfoS("fetching object from key vault", "objectName", keyVaultObject.ObjectName, "objectType", keyVaultObject.ObjectType, "keyvault", p.KeyvaultName, "pod", klog.ObjectRef{Namespace: p.PodNamespace, Name: p.PodName})
		if err := validateObjectFormat(keyVaultObject.ObjectFormat, keyVaultObject.ObjectType); err != nil {
			return nil, nil, wrapObjectTypeError(err, keyVaultObject.ObjectType, keyVaultObject.ObjectName, keyVaultObject.ObjectVersion)
		}
		if err := validateObjectEncoding(keyVaultObject.ObjectEncoding, keyVaultObject.ObjectType); err != nil {
			return nil, nil, wrapObjectTypeError(err, keyVaultObject.ObjectType, keyVaultObject.ObjectName, keyVaultObject.ObjectVersion)
		}
		fileName := keyVaultObject.ObjectName
		if keyVaultObject.ObjectAlias != "" {
			fileName = keyVaultObject.ObjectAlias
		}
		if err := validateFileName(fileName); err != nil {
			return nil, nil, wrapObjectTypeError(err, keyVaultObject.ObjectType, keyVaultObject.ObjectName, keyVaultObject.ObjectVersion)
		}

		// fetch the object from Key Vault
		content, newObjectVersion, err := p.GetKeyVaultObjectContent(ctx, keyVaultObject)
		if err != nil {
			return nil, nil, err
		}

		// objectUID is a unique identifier in the format <object type>/<object name>
		// This is the object id the user sees in the SecretProviderClassPodStatus
		objectUID := getObjectUID(keyVaultObject.ObjectName, keyVaultObject.ObjectType)
		objectVersionMap[objectUID] = newObjectVersion

		objectContent, err := getContentBytes(content, keyVaultObject.ObjectType, keyVaultObject.ObjectEncoding)
		if err != nil {
			return nil, nil, err
		}
		// if the feature to return secrets to CSI driver isn't enabled, the provider will continue to write
		// the contents to the filesystem.
		if !*DriverWriteSecrets {
			if err := os.WriteFile(filepath.Join(targetPath, fileName), objectContent, permission); err != nil {
				return nil, nil, errors.Wrapf(err, "failed to write file %s at %s", fileName, targetPath)
			}
			klog.InfoS("successfully wrote file", "file", fileName, "pod", klog.ObjectRef{Namespace: p.PodNamespace, Name: p.PodName})
		} else {
			// these files will be returned to the CSI driver as part of gRPC response
			files[fileName] = objectContent
			klog.InfoS("added file to the gRPC response", "file", fileName, "pod", klog.ObjectRef{Namespace: p.PodNamespace, Name: p.PodName})
		}
	}

	return files, objectVersionMap, nil
}

// GetKeyVaultObjectContent get content of the keyvault object
func (p *Provider) GetKeyVaultObjectContent(ctx context.Context, kvObject KeyVaultObject) (content, version string, err error) {
	vaultURL, err := p.getVaultURL(ctx)
	if err != nil {
		return "", "", errors.Wrap(err, "failed to get vault")
	}
	kvClient, err := p.initializeKvClient()
	if err != nil {
		return "", "", errors.Wrap(err, "failed to get keyvault client")
	}

	switch kvObject.ObjectType {
	case VaultObjectTypeSecret:
		secret, err := kvClient.GetSecret(ctx, *vaultURL, kvObject.ObjectName, kvObject.ObjectVersion)
		if err != nil {
			return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
		}
		if secret.Value == nil {
			return "", "", errors.Errorf("secret value is nil")
		}
		if secret.ID == nil {
			return "", "", errors.Errorf("secret id is nil")
		}
		content := *secret.Value
		version := getObjectVersion(*secret.ID)
		// if the secret is part of a certificate, then we need to convert the certificate and key to PEM format
		if secret.Kid != nil && len(*secret.Kid) > 0 {
			switch *secret.ContentType {
			case certTypePem:
				return content, version, nil
			case certTypePfx:
				// object format requested is pfx, then return the content as is
				if strings.EqualFold(kvObject.ObjectFormat, objectFormatPFX) {
					return content, version, err
				}
				// convert to pem as that's the default object format for this provider
				content, err := decodePKCS12(*secret.Value)
				if err != nil {
					return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
				}
				return content, version, nil
			default:
				err := errors.Errorf("failed to get certificate. unknown content type '%s'", *secret.ContentType)
				return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
			}
		}
		return content, version, nil
	case VaultObjectTypeKey:
		keybundle, err := kvClient.GetKey(ctx, *vaultURL, kvObject.ObjectName, kvObject.ObjectVersion)
		if err != nil {
			return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
		}
		if keybundle.Key == nil {
			return "", "", errors.Errorf("key value is nil")
		}
		if keybundle.Key.Kid == nil {
			return "", "", errors.Errorf("key id is nil")
		}
		version := getObjectVersion(*keybundle.Key.Kid)
		// for object type "key" the public key is written to the file in PEM format
		switch keybundle.Key.Kty {
		case kv.RSA, kv.RSAHSM:
			// decode the base64 bytes for n
			nb, err := base64.RawURLEncoding.DecodeString(*keybundle.Key.N)
			if err != nil {
				return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
			}
			// decode the base64 bytes for e
			eb, err := base64.RawURLEncoding.DecodeString(*keybundle.Key.E)
			if err != nil {
				return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
			}
			e := new(big.Int).SetBytes(eb).Int64()
			pKey := &rsa.PublicKey{
				N: new(big.Int).SetBytes(nb),
				E: int(e),
			}
			derBytes, err := x509.MarshalPKIXPublicKey(pKey)
			if err != nil {
				return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
			}
			pubKeyBlock := &pem.Block{
				Type:  "PUBLIC KEY",
				Bytes: derBytes,
			}
			var pemData []byte
			pemData = append(pemData, pem.EncodeToMemory(pubKeyBlock)...)
			return string(pemData), version, nil
		case kv.EC, kv.ECHSM:
			// decode the base64 bytes for x
			xb, err := base64.RawURLEncoding.DecodeString(*keybundle.Key.X)
			if err != nil {
				return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
			}
			// decode the base64 bytes for y
			yb, err := base64.RawURLEncoding.DecodeString(*keybundle.Key.Y)
			if err != nil {
				return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
			}
			crv, err := getCurve(keybundle.Key.Crv)
			if err != nil {
				return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
			}
			pKey := &ecdsa.PublicKey{
				X:     new(big.Int).SetBytes(xb),
				Y:     new(big.Int).SetBytes(yb),
				Curve: crv,
			}
			derBytes, err := x509.MarshalPKIXPublicKey(pKey)
			if err != nil {
				return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
			}
			pubKeyBlock := &pem.Block{
				Type:  "PUBLIC KEY",
				Bytes: derBytes,
			}
			var pemData []byte
			pemData = append(pemData, pem.EncodeToMemory(pubKeyBlock)...)
			return string(pemData), version, nil
		default:
			err := errors.Errorf("failed to get key. key type '%s' currently not supported", keybundle.Key.Kty)
			return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
		}
	case VaultObjectTypeCertificate:
		// for object type "cert" the certificate is written to the file in PEM format
		certbundle, err := kvClient.GetCertificate(ctx, *vaultURL, kvObject.ObjectName, kvObject.ObjectVersion)
		if err != nil {
			return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
		}
		if certbundle.Cer == nil {
			return "", "", errors.Errorf("certificate value is nil")
		}
		if certbundle.ID == nil {
			return "", "", errors.Errorf("certificate id is nil")
		}
		version := getObjectVersion(*certbundle.ID)

		certBlock := &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: *certbundle.Cer,
		}
		var pemData []byte
		pemData = append(pemData, pem.EncodeToMemory(certBlock)...)
		return string(pemData), version, nil
	default:
		err := errors.Errorf("Invalid vaultObjectTypes. Should be secret, key, or cert")
		return "", "", wrapObjectTypeError(err, kvObject.ObjectType, kvObject.ObjectName, kvObject.ObjectVersion)
	}
}

func wrapObjectTypeError(err error, objectType, objectName, objectVersion string) error {
	return errors.Wrapf(err, "failed to get objectType:%s, objectName:%s, objectVersion:%s", objectType, objectName, objectVersion)
}

// decodePkcs12 decodes PKCS#12 client certificates by extracting the public certificates, the private
// keys and converts it to PEM format
func decodePKCS12(value string) (content string, err error) {
	pfxRaw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	// using ToPEM to extract more than one certificate and key in pfxData
	pemBlock, err := pkcs12.ToPEM(pfxRaw, "")
	if err != nil {
		return "", err
	}

	var pemKeyData, pemCertData, pemData []byte
	for _, block := range pemBlock {
		// PEM block encoded form contains the headers
		//    -----BEGIN Type-----
		//    Headers
		//    base64-encoded Bytes
		//    -----END Type-----
		// Setting headers to nil to ensure no headers included in the encoded block
		block.Headers = make(map[string]string)
		if block.Type == certificateType {
			pemCertData = append(pemCertData, pem.EncodeToMemory(block)...)
		} else {
			key, err := parsePrivateKey(block.Bytes)
			if err != nil {
				return "", err
			}
			// pkcs1 RSA private key PEM file is specific for RSA keys. RSA is not used exclusively inside X509
			// and SSL/TLS, a more generic key format is available in the form of PKCS#8 that identifies the type
			// of private key and contains the relevant data.
			// Converting to pkcs8 private key as ToPEM uses pkcs1
			// The driver determines the key type from the pkcs8 form of the key and marshals appropriately
			block.Bytes, err = x509.MarshalPKCS8PrivateKey(key)
			if err != nil {
				return "", err
			}
			pemKeyData = append(pemKeyData, pem.EncodeToMemory(block)...)
		}
	}

	// construct the pem chain in the order
	// SERVER, INTERMEDIATE, ROOT
	if *ConstructPEMChain {
		pemCertData, err = fetchCertChains(pemCertData)
		if err != nil {
			return "", err
		}
	}

	pemData = append(pemData, pemKeyData...)
	pemData = append(pemData, pemCertData...)
	return string(pemData), nil
}

func getCurve(crv kv.JSONWebKeyCurveName) (elliptic.Curve, error) {
	switch crv {
	case kv.P256:
		return elliptic.P256(), nil
	case kv.P384:
		return elliptic.P384(), nil
	case kv.P521:
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("curve %s is not suppported", crv)
	}
}

func parsePrivateKey(block []byte) (interface{}, error) {
	if key, err := x509.ParsePKCS1PrivateKey(block); err == nil {
		return key, nil
	}
	if key, err := x509.ParsePKCS8PrivateKey(block); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block); err == nil {
		return key, nil
	}
	return nil, fmt.Errorf("failed to parse key for type pkcs1, pkcs8 or ec")
}

// setAzureEnvironmentFilePath sets the AZURE_ENVIRONMENT_FILEPATH env var which is used by
// go-autorest for AZURESTACKCLOUD
func setAzureEnvironmentFilePath(envFileName string) error {
	if envFileName == "" {
		return nil
	}
	klog.Infof("setting AZURE_ENVIRONMENT_FILEPATH to %s for custom cloud", envFileName)
	return os.Setenv(azure.EnvironmentFilepathName, envFileName)
}

// validateObjectFormat checks if the object format is valid and is supported
// for the given object type
func validateObjectFormat(objectFormat, objectType string) error {
	if len(objectFormat) == 0 {
		return nil
	}
	if !strings.EqualFold(objectFormat, objectFormatPEM) && !strings.EqualFold(objectFormat, objectFormatPFX) {
		return fmt.Errorf("invalid objectFormat: %v, should be PEM or PFX", objectFormat)
	}
	// Azure Key Vault returns the base64 encoded binary content only for type secret
	// for types cert/key, the content is always in pem format
	if objectFormat == objectFormatPFX && objectType != VaultObjectTypeSecret {
		return fmt.Errorf("PFX format only supported for objectType: secret")
	}
	return nil
}

// getObjectVersion parses the id to retrieve the version
// of object fetched
// example id format - https://kindkv.vault.azure.net/secrets/actual/1f304204f3624873aab40231241243eb
// TODO (aramase) follow up on https://github.com/Azure/azure-rest-api-specs/issues/10825 to provide
// a native way to obtain the version
func getObjectVersion(id string) string {
	splitID := strings.Split(id, "/")
	return splitID[len(splitID)-1]
}

// getObjectUID returns UID for the object with the format
// <object type>/<object name>
func getObjectUID(objectName, objectType string) string {
	return fmt.Sprintf("%s/%s", objectType, objectName)
}

// validateObjectEncoding checks if the object encoding is valid and is supported
// for the given object type
func validateObjectEncoding(objectEncoding, objectType string) error {
	if len(objectEncoding) == 0 {
		return nil
	}

	// ObjectEncoding is supported only for secret types
	if objectType != VaultObjectTypeSecret {
		return fmt.Errorf("objectEncoding only supported for objectType: secret")
	}

	if !strings.EqualFold(objectEncoding, objectEncodingHex) && !strings.EqualFold(objectEncoding, objectEncodingBase64) && !strings.EqualFold(objectEncoding, objectEncodingUtf8) {
		return fmt.Errorf("invalid objectEncoding: %v, should be hex, base64 or utf-8", objectEncoding)
	}

	return nil
}

// getContentBytes takes the given content string and returns the bytes to write to disk
// If an encoding is specified it will decode the string first
func getContentBytes(content, objectType, objectEncoding string) ([]byte, error) {
	if !strings.EqualFold(objectType, VaultObjectTypeSecret) || len(objectEncoding) == 0 || strings.EqualFold(objectEncoding, objectEncodingUtf8) {
		return []byte(content), nil
	}

	if strings.EqualFold(objectEncoding, objectEncodingBase64) {
		return base64.StdEncoding.DecodeString(content)
	}

	if strings.EqualFold(objectEncoding, objectEncodingHex) {
		return hex.DecodeString(content)
	}

	return make([]byte, 0), fmt.Errorf("invalid objectEncoding. Should be utf-8, base64, or hex")
}

// formatKeyVaultObject formats the fields in KeyVaultObject
func formatKeyVaultObject(object *KeyVaultObject) {
	if object == nil {
		return
	}
	objectPtr := reflect.ValueOf(object)
	objectValue := objectPtr.Elem()

	for i := 0; i < objectValue.NumField(); i++ {
		field := objectValue.Field(i)
		if field.Type() != reflect.TypeOf("") {
			continue
		}
		str := field.Interface().(string)
		str = strings.TrimSpace(str)
		field.SetString(str)
	}
}

// This validate will make sure fileName:
// 1. is not abs path
// 2. does not contain any '..' elements
// 3. does not start with '..'
// These checks have been implemented based on -
// [validateLocalDescendingPath] https://github.com/kubernetes/kubernetes/blob/master/pkg/apis/core/validation/validation.go#L1158-L1170
// [validatePathNoBacksteps] https://github.com/kubernetes/kubernetes/blob/master/pkg/apis/core/validation/validation.go#L1172-L1186
func validateFileName(fileName string) error {
	if len(fileName) == 0 {
		return fmt.Errorf("file name must not be empty")
	}
	// is not abs path
	if filepath.IsAbs(fileName) {
		return fmt.Errorf("file name must be a relative path")
	}
	// does not have any element which is ".."
	parts := strings.Split(filepath.ToSlash(fileName), "/")
	for _, item := range parts {
		if item == ".." {
			return fmt.Errorf("file name must not contain '..'")
		}
	}
	// fallback logic if .. is missed in the previous check
	if strings.Contains(fileName, "..") {
		return fmt.Errorf("file name must not contain '..'")
	}
	return nil
}

type node struct {
	cert     *x509.Certificate
	parent   *node
	isParent bool
}

func fetchCertChains(data []byte) ([]byte, error) {
	var newCertChain []*x509.Certificate
	var pemData []byte
	nodes := make([]*node, 0)

	for {
		// decode pem to der first
		block, rest := pem.Decode(data)
		data = rest

		if block == nil {
			break
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return pemData, err
		}
		// this should not be the case because ParseCertificate should return a non nil
		// certificate when there is no error.
		if cert == nil {
			return pemData, fmt.Errorf("certificate is nil")
		}
		nodes = append(nodes, &node{
			cert:     cert,
			parent:   nil,
			isParent: false,
		})
	}

	// at the end of this computation, the output will be a single linked list
	// the tail of the list will be the root node (which has no parents)
	// the head of the list will be the leaf node (whose parent will be intermediate certs)
	// (head) leaf -> intermediates -> root (tail)
	for i := range nodes {
		for j := range nodes {
			// ignore same node to prevent generating a cycle
			if i == j {
				continue
			}
			// if ith node AuthorityKeyId is same as jth node SubjectKeyId, jth node was used
			// to sign the ith certificate
			if string(nodes[i].cert.AuthorityKeyId) == string(nodes[j].cert.SubjectKeyId) {
				nodes[j].isParent = true
				nodes[i].parent = nodes[j]
				break
			}
		}
	}

	var leaf *node
	for i := range nodes {
		if !nodes[i].isParent {
			// this is the leaf node as it's not a parent for any other node
			// TODO (aramase) handle errors if there are more than 1 leaf nodes
			leaf = nodes[i]
			break
		}
	}

	if leaf == nil {
		return nil, fmt.Errorf("no leaf found")
	}

	processedNodes := 0
	// iterate through the directed list and append the nodes to new cert chain
	for leaf != nil {
		processedNodes++
		// ensure we aren't stuck in a cyclic loop
		if processedNodes > len(nodes) {
			return pemData, fmt.Errorf("constructing chain resulted in cycle")
		}
		newCertChain = append(newCertChain, leaf.cert)
		leaf = leaf.parent
	}

	for _, cert := range newCertChain {
		b := &pem.Block{
			Type:  certificateType,
			Bytes: cert.Raw,
		}
		pemData = append(pemData, pem.EncodeToMemory(b)...)
	}
	return pemData, nil
}
