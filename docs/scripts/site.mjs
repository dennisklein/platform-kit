// Where the site and its repository live. GitHub Actions sets GITHUB_REPOSITORY, so a
// build in a fork links to the fork. A local build uses the default below.
const slug = process.env.GITHUB_REPOSITORY || 'dennisklein/platform-kit';
const [owner, repo] = slug.split('/');

/** The repository on GitHub, for example "https://github.com/dennisklein/platform-kit". */
export const repoUrl = `https://github.com/${slug}`;
/** The GitHub Pages host of the repository owner. */
export const site = `https://${owner.toLowerCase()}.github.io`;
/** The path of the site on the host. */
export const base = `/${repo}`;
/** The branch that the site documents and deploys from. */
export const branch = 'metal';
