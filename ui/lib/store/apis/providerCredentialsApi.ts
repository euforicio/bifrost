import {
	ListProviderCredentialsResponse,
	ProviderCredentialLoginStatus,
	ProviderCredentialStatus,
	ProviderCredentialUsage,
} from "@/lib/types/config";
import { baseApi } from "./baseApi";

interface ProviderCredentialRequest {
	provider: string;
	keyId: string;
}

interface ProviderCredentialLoginRequest extends ProviderCredentialRequest {
	loginId: string;
}

const credentialPath = ({ provider, keyId }: ProviderCredentialRequest) =>
	`/providers/${encodeURIComponent(provider)}/credentials/${encodeURIComponent(keyId)}`;

export const providerCredentialsApi = baseApi.injectEndpoints({
	endpoints: (builder) => ({
		getProviderCredentials: builder.query<ProviderCredentialStatus[], string>({
			query: (provider) => `/providers/${encodeURIComponent(provider)}/credentials`,
			transformResponse: (response: ListProviderCredentialsResponse) => response.credentials ?? [],
			providesTags: (result, error, provider) => [{ type: "ProviderCredentials", id: provider }],
		}),
		startProviderCredentialLogin: builder.mutation<ProviderCredentialLoginStatus, ProviderCredentialRequest>({
			query: (request) => ({
				url: `${credentialPath(request)}/login/${request.provider === "cursor" ? "browser" : "device"}`,
				method: "POST",
			}),
		}),
		getProviderCredentialLogin: builder.query<ProviderCredentialLoginStatus, ProviderCredentialLoginRequest>({
			query: (request) => `${credentialPath(request)}/login/${encodeURIComponent(request.loginId)}`,
		}),
		cancelProviderCredentialLogin: builder.mutation<void, ProviderCredentialLoginRequest>({
			query: (request) => ({
				url: `${credentialPath(request)}/login/${encodeURIComponent(request.loginId)}`,
				method: "DELETE",
			}),
		}),
		getProviderCredentialStatus: builder.query<ProviderCredentialStatus, ProviderCredentialRequest>({
			query: (request) => `${credentialPath(request)}/status`,
			providesTags: (result, error, { provider }) => [{ type: "ProviderCredentials", id: provider }],
		}),
		getProviderCredentialUsage: builder.query<ProviderCredentialUsage, ProviderCredentialRequest>({
			query: (request) => `${credentialPath(request)}/usage`,
			providesTags: (result, error, { provider, keyId }) => [
				{ type: "ProviderCredentials", id: provider },
				{ type: "ProviderCredentials", id: `${provider}:${keyId}:usage` },
			],
		}),
		refreshProviderCredential: builder.mutation<ProviderCredentialStatus, ProviderCredentialRequest>({
			query: (request) => ({
				url: `${credentialPath(request)}/refresh`,
				method: "POST",
			}),
			invalidatesTags: (result, error, { provider }) => [{ type: "ProviderCredentials", id: provider }],
		}),
		disconnectProviderCredential: builder.mutation<void, ProviderCredentialRequest>({
			query: (request) => ({
				url: credentialPath(request),
				method: "DELETE",
			}),
			invalidatesTags: (result, error, { provider }) => [{ type: "ProviderCredentials", id: provider }],
		}),
	}),
});

export const {
	useCancelProviderCredentialLoginMutation,
	useDisconnectProviderCredentialMutation,
	useGetProviderCredentialLoginQuery,
	useGetProviderCredentialsQuery,
	useGetProviderCredentialStatusQuery,
	useGetProviderCredentialUsageQuery,
	useRefreshProviderCredentialMutation,
	useStartProviderCredentialLoginMutation,
} = providerCredentialsApi;