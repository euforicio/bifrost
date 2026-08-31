import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";
import { getErrorMessage } from "@/lib/store";
import { useGetProviderCredentialUsageQuery } from "@/lib/store/apis/providerCredentialsApi";
import { ProviderUsageQuota } from "@/lib/types/config";
import { AlertCircle, CircleDollarSign, Gauge, Loader2, RefreshCw, RotateCcw } from "lucide-react";

interface Props {
	credentialId: string;
	provider: string;
}

const numberFormatter = new Intl.NumberFormat("en-US", { maximumFractionDigits: 2 });
const moneyFormatter = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD" });

function formatTimestamp(value?: string) {
	if (!value) return null;
	const date = new Date(value);
	if (Number.isNaN(date.getTime())) return null;
	return date.toLocaleString();
}

function formatWindow(minutes?: number) {
	if (!minutes || minutes <= 0) return null;
	if (minutes % 1440 === 0) return `${numberFormatter.format(minutes / 1440)} ${minutes === 1440 ? "day" : "days"}`;
	if (minutes % 60 === 0) return `${numberFormatter.format(minutes / 60)} ${minutes === 60 ? "hour" : "hours"}`;
	return `${numberFormatter.format(minutes)} ${minutes === 1 ? "minute" : "minutes"}`;
}

function formatQuotaValue(value: number, unit?: string) {
	const normalizedUnit = unit?.toLowerCase();
	if (normalizedUnit === "cent" || normalizedUnit === "cents") return moneyFormatter.format(value / 100);
	const formatted = numberFormatter.format(value);
	if (!unit) return formatted;
	if (normalizedUnit === "request" || normalizedUnit === "requests") return `${formatted} ${value === 1 ? "request" : "requests"}`;
	if (normalizedUnit === "token" || normalizedUnit === "tokens") return `${formatted} ${value === 1 ? "token" : "tokens"}`;
	return `${formatted} ${unit}`;
}

function quotaDetails(quota: ProviderUsageQuota) {
	const details: string[] = [];
	if (quota.used !== undefined) details.push(`${formatQuotaValue(quota.used, quota.unit)} used`);
	if (quota.remaining !== undefined) details.push(`${formatQuotaValue(quota.remaining, quota.unit)} remaining`);
	if (quota.limit !== undefined) details.push(`${formatQuotaValue(quota.limit, quota.unit)} limit`);
	return details.join(" · ");
}

function safeTestId(value: string) {
	return value.replace(/[^a-z0-9]+/gi, "-").toLowerCase();
}

function UsageQuota({ quota }: { quota: ProviderUsageQuota }) {
	const resetAt = formatTimestamp(quota.resets_at);
	const startsAt = formatTimestamp(quota.starts_at);
	const window = formatWindow(quota.window_duration_minutes);
	const details = quotaDetails(quota);
	const percent = quota.used_percent === undefined ? null : Math.max(0, Math.min(quota.used_percent, 100));
	const testId = safeTestId(quota.id);

	return (
		<div className="bg-background min-w-0 rounded-sm border p-3" data-testid={`provider-usage-quota-${testId}`}>
			<div className="flex items-start justify-between gap-3">
				<div className="min-w-0">
					<p className="truncate text-sm font-medium">{quota.name}</p>
					{window ? <p className="text-muted-foreground text-xs">{window} window</p> : null}
				</div>
				{percent !== null ? <span className="shrink-0 text-sm font-medium">{numberFormatter.format(percent)}%</span> : null}
			</div>
			{percent !== null ? (
				<Progress
					value={percent}
					className="mt-2 h-1.5"
					aria-label={`${quota.name} usage`}
					aria-valuetext={`${numberFormatter.format(percent)} percent used`}
					data-testid={`provider-usage-progress-${testId}`}
				/>
			) : null}
			{details ? <p className="text-muted-foreground mt-2 text-xs">{details}</p> : null}
			{startsAt || resetAt ? (
				<p className="text-muted-foreground mt-1 text-xs">
					{startsAt ? `Started ${startsAt}` : null}
					{startsAt && resetAt ? " · " : null}
					{resetAt ? `Resets ${resetAt}` : null}
				</p>
			) : null}
		</div>
	);
}

export default function ProviderUsageSummary({ credentialId, provider }: Props) {
	const { data, error, isLoading, isFetching, refetch } = useGetProviderCredentialUsageQuery({
		provider,
		keyId: credentialId,
	});
	const fetchedAt = formatTimestamp(data?.fetched_at);

	if (isLoading) {
		return (
			<div
				className="text-muted-foreground flex items-center gap-2 border-t pt-3 text-xs"
				data-testid={`provider-usage-loading-${credentialId}`}
			>
				<Loader2 className="h-3.5 w-3.5 animate-spin" /> Loading account usage
			</div>
		);
	}

	if (!data && error) {
		return (
			<div
				className="border-destructive/30 bg-destructive/5 flex flex-wrap items-center justify-between gap-2 rounded-sm border p-3 text-sm"
				data-testid={`provider-usage-error-${credentialId}`}
			>
				<span className="flex min-w-0 items-center gap-2">
					<AlertCircle className="text-destructive h-4 w-4 shrink-0" />
					<span className="truncate">Could not load account usage: {getErrorMessage(error)}</span>
				</span>
				<Button
					variant="outline"
					size="sm"
					onClick={() => refetch()}
					disabled={isFetching}
					data-testid={`provider-usage-refresh-${credentialId}`}
				>
					<RefreshCw className={`h-3.5 w-3.5 ${isFetching ? "animate-spin" : ""}`} /> Retry
				</Button>
			</div>
		);
	}

	if (!data) return null;

	const unsupportedMessage = "Provider does not expose account usage through its API";
	const showCredits = data.credits !== undefined;

	return (
		<section className="border-t pt-3" aria-label="Provider account usage" data-testid={`provider-usage-summary-${credentialId}`}>
			<div className="flex flex-wrap items-center justify-between gap-2">
				<div className="flex min-w-0 flex-wrap items-center gap-2">
					<span className="flex items-center gap-1.5 text-sm font-medium">
						<Gauge className="h-4 w-4" /> Account usage
					</span>
					{data.stale ? (
						<Badge variant="warning" data-testid={`provider-usage-stale-${credentialId}`}>
							Stale
						</Badge>
					) : null}
					{fetchedAt ? <span className="text-muted-foreground text-xs">Updated {fetchedAt}</span> : null}
				</div>
				<Button
					variant="ghost"
					size="sm"
					onClick={() => refetch()}
					disabled={isFetching}
					aria-label="Refresh account usage"
					data-testid={`provider-usage-refresh-${credentialId}`}
				>
					<RefreshCw className={`h-3.5 w-3.5 ${isFetching ? "animate-spin" : ""}`} /> Refresh usage
				</Button>
			</div>

			{error ? (
				<p className="text-destructive mt-2 flex items-center gap-1.5 text-xs" role="status">
					<AlertCircle className="h-3.5 w-3.5" /> Could not refresh usage: {getErrorMessage(error)}
				</p>
			) : null}

			{data.availability === "unsupported" ? (
				<p className="text-muted-foreground mt-2 text-sm" data-testid={`provider-usage-unsupported-${credentialId}`}>
					{data.message || unsupportedMessage}
				</p>
			) : data.availability === "unavailable" ? (
				<p className="text-muted-foreground mt-2 text-sm" data-testid={`provider-usage-unavailable-${credentialId}`}>
					{data.message || "Account usage is temporarily unavailable."}
				</p>
			) : (
				<>
					{data.message ? <p className="text-muted-foreground mt-2 text-xs">{data.message}</p> : null}
					{data.quotas.length > 0 ? (
						<div className="mt-3 grid gap-2 sm:grid-cols-2 xl:grid-cols-3" data-testid={`provider-usage-quotas-${credentialId}`}>
							{data.quotas.map((quota) => (
								<UsageQuota key={quota.id} quota={quota} />
							))}
						</div>
					) : null}

					{showCredits ? (
						<div
							className="bg-background mt-3 flex items-center gap-2 rounded-sm border p-3 text-sm"
							data-testid={`provider-usage-credits-${credentialId}`}
						>
							<CircleDollarSign className="text-muted-foreground h-4 w-4 shrink-0" />
							<span className="font-medium">
								{data.credits?.unlimited
									? "Unlimited credits"
									: !data.credits?.has_credits
										? "No account credits available"
										: data.credits.balance !== undefined
											? `${numberFormatter.format(data.credits.balance)} credits remaining`
											: "Account credits available"}
							</span>
						</div>
					) : null}

					{data.reset_credits ? (
						<div className="bg-background mt-3 rounded-sm border p-3" data-testid={`provider-usage-resets-${credentialId}`}>
							<div className="flex items-center gap-2 text-sm font-medium">
								<RotateCcw className="text-muted-foreground h-4 w-4" /> {data.reset_credits.available_count}{" "}
								{data.reset_credits.available_count === 1 ? "reset" : "resets"} available
							</div>
							{data.reset_credits.credits.length > 0 ? (
								<div className="mt-2 grid gap-2 sm:grid-cols-2">
									{data.reset_credits.credits.map((credit) => {
										const expiresAt = formatTimestamp(credit.expires_at);
										const grantedAt = formatTimestamp(credit.granted_at);
										return (
											<div
												className="bg-muted/40 rounded-sm p-2.5"
												key={credit.id}
												data-testid={`provider-usage-reset-${safeTestId(credit.id)}`}
											>
												<div className="flex flex-wrap items-center justify-between gap-2">
													<span className="text-sm font-medium">{credit.title || credit.reset_type}</span>
													<Badge variant="secondary">{credit.status}</Badge>
												</div>
												{credit.description ? <p className="text-muted-foreground mt-1 text-xs">{credit.description}</p> : null}
												{grantedAt || expiresAt ? (
													<p className="text-muted-foreground mt-1 text-xs">
														{grantedAt ? `Granted ${grantedAt}` : null}
														{grantedAt && expiresAt ? " · " : null}
														{expiresAt ? `Expires ${expiresAt}` : null}
													</p>
												) : null}
											</div>
										);
									})}
								</div>
							) : null}
						</div>
					) : null}
				</>
			)}
		</section>
	);
}