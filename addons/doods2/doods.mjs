import Hls from "./static/scripts/vendor/hls.mjs";
import { $, uniqueID } from "./static/scripts/libs/common.mjs";
import { newFeed } from "./static/scripts/components/feed.mjs";
import { newModal } from "./static/scripts/components/modal.mjs";

// CSS.
let $style = document.createElement("style");
$style.type = "text/css";
$style.innerHTML = `
	.doods-label-wrapper {
		display: flex;
		padding: 0.1rem;
		border-top-style: solid;
		border-color: var(--color1);
		border-width: 0.03rem;
		align-items: center;
	}
	.doods-label-wrapper:first-child {
		border-top-style: none;
	}
	.doods-label {
		font-size: 0.7rem;
		color: var(--color-text);
	}
	.doods-threshold {
		margin-left: auto;
		font-size: 0.6rem;
		text-align: center;
		width: 1.4rem;
		height: 100%;
	}

	/* Crop. */
	.doodsCrop-preview-feed {
		width: 100%;
		min-width: 0;
		display: flex;
		background: black;
	}
	.doodsCrop-preview-overlay {
		position: absolute;
		height: 100%;
		width: 100%;
		top: 0;
	}
	.doodsCrop-option-wrapper {
		display: flex;
		flex-wrap: wrap;
	}
	.doodsCrop-option {
		display: flex;
		background: var(--color2);
		padding: 0.15rem;
		border-radius: 0.15rem;
		margin-right: 0.2rem;
		margin-bottom: 0.2rem;
	}
	.doodsCrop-option-label {
		font-size: 0.7rem;
		color: var(--color-text);
		margin-left: 0.1rem;
		margin-right: 0.2rem;
	}
	.doodsCrop-option-input {
		text-align: center;
		font-size: 0.5rem;
		border-style: none;
		border-radius: 5px;
		width: 1.4rem;
	}

	/* Mask. */
	.doodsMask-preview-feed {
		width: 100%;
		min-width: 0;
		display: flex;
		background: black;
	}
	.doodsMask-preview-overlay {
		position: absolute;
		height: 100%;
		width: 100%;
		top: 0;
	}
	.doodsMask-points-grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(3.6rem, 3.7rem));
		column-gap: 0.1rem;
		row-gap: 0.1rem;
	}
	.doodsMask-point {
		display: flex;
		background: var(--color2);
		padding: 0.15rem;
		border-radius: 0.15rem;
	}
	.doodsMask-point-label {
		font-size: 0.7rem;
		color: var(--color-text);
		margin-left: 0.1rem;
		margin-right: 0.1rem;
	}
	.doodsMask-point-input {
		text-align: center;
		font-size: 0.5rem;
		border-style: none;
		border-radius: 5px;
		min-width: 0;
	}
	.doodsMask-button {
		background: var(--color2);
	}
	.doodsMask-button:hover {
		background: var(--color1);
	}`;

$("head").append($style);

const Detectors = JSON.parse(`$detectorsJSON`);

function doodsThresholds() {
	return thresholds(Detectors);
}

function thresholds(detectors) {
	const detectorByName = (name) => {
		for (const detector of detectors) {
			if (detector.name === name) {
				return detector;
			}
		}
	};

	const newField = (label, val) => {
		const id = uniqueID();
		return {
			html: `
				<li class="doods-label-wrapper">
					<label for="${id}" class="doods-label">${label}</label>
					<input
						id="${id}"
						class="doods-threshold"
						type="number"
						value="${val}"
					/>
				</li>`,
			value() {
				return $(`#${id}`).value;
			},
			label() {
				return label;
			},
			validate(input) {
				if (0 > input) {
					return "min value: 0";
				} else if (input > 100) {
					return "max value: 100";
				} else {
					return "";
				}
			},
		};
	};

	let value, modal, fields, $modalContent, validateErr;
	let isRendered = false;
	const render = (element) => {
		if (isRendered) {
			return;
		}
		modal = newModal("DOODS thresholds");
		element.insertAdjacentHTML("beforeend", modal.html);
		$modalContent = modal.init(element);

		modal.onClose(() => {
			// Get value.
			value = {};
			for (const field of fields) {
				value[field.label()] = Number(field.value());
			}

			// Validate fields.
			validateErr = "";
			for (const field of fields) {
				const err = field.validate(field.value());
				if (err != "") {
					validateErr = `"DOODS thresholds": "${field.label()}": ${err}`;
					break;
				}
			}
		});
		isRendered = true;
	};

	const defaultThresh = 50;

	const setValue = (detectorName) => {
		// Get labels from detector.
		let labelNames = detectorByName(detectorName).labels;

		var labels = {};
		for (const name of labelNames) {
			labels[name] = defaultThresh;
		}

		// Fill in saved values.
		for (const name of Object.keys(value)) {
			if (labels[name]) {
				labels[name] = value[name];
			}
		}

		// Sort keys.
		let labelKeys = [];
		for (const key of Object.keys(labels)) {
			labelKeys.push(key);
		}
		labelKeys.sort();

		fields = [];

		// Create fields
		for (const name of labelKeys) {
			fields.push(newField(name, labels[name]));
		}

		// Render fields.
		let html = "";
		for (const field of fields) {
			html += field.html;
		}
		$modalContent.innerHTML = html;
	};

	let monitorFields;
	const id = uniqueID();

	return {
		html: `
			<li
				id="${id}"
				class="form-field"
				style="display:flex; padding-bottom:0.25rem;"
			>
				<label class="form-field-label">DOODS thresholds</label>
				<div style="width:auto">
					<button class="settings-edit-btn color3">
						<img src="static/icons/feather/edit-3.svg"/>
					</button>
				</div>
			</li> `,
		value() {
			return JSON.stringify(value);
		},
		set(input, _, f) {
			value = input ? JSON.parse(input) : {};
			validateErr = "";
			monitorFields = f;
		},
		validate() {
			return validateErr;
		},
		init($parent) {
			const element = $parent.querySelector("#" + id);
			element.querySelector(".settings-edit-btn").addEventListener("click", () => {
				const detectorName = monitorFields.doodsDetectorName.value();
				if (detectorName === "") {
					alert("please select a detector");
					return;
				}

				render(element);
				setValue(detectorName);
				modal.open();
			});
		},
	};
}

function doodsCrop() {
	return crop(Hls, Detectors);
}

function crop(hls, detectors) {
	const detectorAspectRatio = (name) => {
		for (const detector of detectors) {
			if (detector.name === name) {
				return detector["width"] / detector["height"];
			}
		}
	};

	let fields = {};
	let value = [];
	let $wrapper, $padding, $x, $y, $size, $modalContent, $feed;

	const modal = newModal("DOODS crop");

	const renderModal = (element, feed) => {
		const html = `
			<li id="doodsCrop-preview" class="form-field">
				<label class="form-field-label" for="doodsCrop-preview" style="width: auto;">Preview</label>
				<div class="js-preview-wrapper" style="position: relative; margin-top: 0.69rem">
					<div class="js-feed doodsCrop-preview-feed">
						${feed.html}
					</div>
					<div class="js-preview-padding" style="background: white;"></div>
					<svg
						class="js-overlay doodsCrop-preview-overlay"
						viewBox="0 0 100 100"
						preserveAspectRatio="none"
						style="opacity: 0.7;"
					></svg>
				</div>
			</li>
			<li
				class="js-options form-field doodsCrop-option-wrapper"
			>
				<div class="js-doodsCrop-option doodsCrop-option">
					<span class="doodsCrop-option-label">X</span>
					<input
						class="js-x doodsCrop-option-input"
						type="number"
						min="0"
						max="100"
						value="0"
					/>
				</div>
				<div class="js-doodsCrop-option doodsCrop-option">
					<span class="doodsCrop-option-label">Y</span>
					<input
						class="js-y doodsCrop-option-input"
						type="number"
						min="0"
						max="100"
						value="0"
					/>
				</div>
				<div class="js-doodsCrop-option doodsCrop-option">
					<span class="doodsCrop-option-label">size</span>
					<input
						class="js-size doodsCrop-option-input"
						type="number"
						min="0"
						max="100"
						value="0"
					/>
				</div>
			</li>`;

		$modalContent = modal.init(element);
		$modalContent.innerHTML = html;

		$feed = $modalContent.querySelector(".js-feed");
		$wrapper = $modalContent.querySelector(".js-preview-wrapper");
		$padding = $modalContent.querySelector(".js-preview-padding");
		$x = $modalContent.querySelector(".js-x");
		$y = $modalContent.querySelector(".js-y");
		$size = $modalContent.querySelector(".js-size");

		set(value);

		// Update padding if $feed size changes. TODO
		// eslint-disable-next-line compat/compat
		new ResizeObserver(updatePadding).observe($feed);

		const $overlay = $modalContent.querySelector(".js-overlay");
		$modalContent.querySelector(".js-options").addEventListener("change", () => {
			$overlay.innerHTML = updatePreview();
		});
		$overlay.innerHTML = updatePreview();
	};

	const updatePadding = () => {
		const detectorName = fields.doodsDetectorName.value();
		if (detectorName === "") {
			alert("please select a detector");
			return;
		}

		const inputWidth = $feed.clientWidth;
		const inputHeight = $feed.clientHeight;
		const inputRatio = inputWidth / inputHeight;
		const outputRatio = detectorAspectRatio(detectorName);

		if (inputRatio > outputRatio) {
			const paddingHeight = inputWidth * outputRatio - inputHeight;
			$wrapper.style.display = "block";
			$padding.style.width = "auto";
			$padding.style.height = paddingHeight + "px";
		} else {
			const paddingWidth = inputHeight * outputRatio - inputWidth;
			$wrapper.style.display = "flex";
			$padding.style.width = paddingWidth + "px";
			$padding.style.height = "auto";
		}
	};

	const updatePreview = () => {
		const x = Number($x.value);
		const y = Number($y.value);
		let s = Number($size.value);

		const max = Math.max(x, y);
		if (max + s > 100) {
			s = 100 - max;
			$size.value = s;
		}

		return `
			<path
				fill-rule="evenodd"
				d="m 0 0 L 100 0 L 100 100 L 0 100 L 0 0 M ${x} ${y} L ${x + s} ${y} L ${x + s} ${
			y + s
		} L ${x} ${y + s} L ${x} ${y}"
			/>`;
	};

	const set = (input) => {
		value = input;
		$x.value = input[0];
		$y.value = input[1];
		$size.value = input[2];
	};

	let rendered = false;
	const id = uniqueID();

	return {
		html: `
			<li
				id="${id}"
				class="form-field"
				style="display:flex; padding-bottom:0.25rem;"
			>
				<label class="form-field-label">DOODS crop</label>
				<div style="width:auto">
					<button class="settings-edit-btn color3">
						<img src="static/icons/feather/edit-3.svg"/>
					</button>
				</div>
				${modal.html}
			</li>`,

		value() {
			if (!rendered) {
				return JSON.stringify(value);
			}
			return JSON.stringify([
				Number($x.value),
				Number($y.value),
				Number($size.value),
			]);
		},
		set(input, _, f) {
			fields = f;
			value = input === "" ? [0, 0, 100] : JSON.parse(input);
			if (rendered) {
				set(value);
			}
		},
		init($parent) {
			var feed;
			const element = $parent.querySelector("#" + id);
			element.querySelector(".settings-edit-btn").addEventListener("click", () => {
				if (!rendered) {
					const subInputEnabled = fields.subInput.value() !== "" ? "true" : "";
					const monitor = {
						id: fields.id.value(),
						audioEnabled: "false",
						subInputEnabled: subInputEnabled,
					};
					feed = newFeed(monitor, true, hls);

					renderModal(element, feed);
					rendered = true;
				}
				modal.open();

				feed.init($modalContent);

				modal.onClose(() => {
					feed.destroy();
				});
			});
		},
	};
}

function doodsMask() {
	return mask(Hls);
}

function mask(hls) {
	let fields = {};
	let value = {};
	let $enable, $overlay, $points, $modalContent;

	const modal = newModal("DOODS mask");

	const renderModal = (element, feed) => {
		const html = `
			<li class="js-enable doodsMask-enabled form-field">
				<label class="form-field-label" for="doodsMask-enable">Enable</label>
				<div class="form-field-select-container">
					<select id="modal-enable" class="form-field-select js-input">
						<option>true</option>
						<option>false</option>
					</select>
				</div>
			</li>
			<li id="doodsMask-preview" class="form-field">
				<label class="form-field-label" for="doodsMask-preview">Preview</label>
				<div class="js-preview-wrapper" style="position: relative; margin-top: 0.69rem">
					<div class="js-feed doodsCrop-preview-feed">${feed.html}</div>
					<svg
						class="js-overlay doodsMask-preview-overlay"
						viewBox="0 0 100 100"
						preserveAspectRatio="none"
						style="opacity: 0.7;"
					></svg>
				</div>
			</li>
			<li class="js-points form-field doodsMask-points-grid"></li>`;

		$modalContent = modal.init(element);
		$modalContent.innerHTML = html;

		$enable = $modalContent.querySelector(".js-enable .js-input");
		$enable.addEventListener("change", () => {
			value.enable = $enable.value == "true";
		});

		$overlay = $modalContent.querySelector(".js-overlay");
		$points = $modalContent.querySelector(".js-points");

		renderValue();
		renderPreview();
	};

	const renderPreview = () => {
		let points = "";
		for (const p of value.area) {
			points += p[0] + "," + p[1] + " ";
		}
		$overlay.innerHTML = `
				<polygon
					style="fill: black;"
					points="${points}"
				/>`;
	};

	const renderValue = () => {
		$enable.value = value.enable;

		let html = "";
		for (const point of Object.entries(value.area)) {
			const index = point[0];
			const [x, y] = point[1];
			html += `
				<div class="js-point doodsMask-point">
					<input
						class="doodsMask-point-input"
						type="number"
						min="0"
						max="100"
						value="${x}"
					/>
					<span class="doodsMask-point-label">${index}</span>
					<input
						class="doodsMask-point-input"
						type="number"
						min="0"
						max="100"
						value="${y}"
					/>
				</div>`;
		}
		html += `
			<div style="display: flex; column-gap: 0.2rem;">
				<button
					class="js-plus settings-edit-btn doodsMask-button"
					style="margin: 0;"
				>
					<img src="static/icons/feather/plus.svg">
				</button>
				<button
					class="js-minus settings-edit-btn doodsMask-button"
					style="margin: 0;"
				>
					<img src="static/icons/feather/minus.svg">
				</button>
			</div>`;

		$points.innerHTML = html;

		for (const element of $points.querySelectorAll(".js-point")) {
			element.addEventListener("change", () => {
				const index = element.querySelector("span").innerHTML;
				const $points = element.querySelectorAll("input");
				const x = Number.parseInt($points[0].value);
				const y = Number.parseInt($points[1].value);
				value.area[index] = [x, y];
				renderPreview();
			});
		}

		$points.querySelector(".js-plus").addEventListener("click", () => {
			value.area.push([50, 50]);
			renderValue();
		});
		$points.querySelector(".js-minus").addEventListener("click", () => {
			if (value.area.length > 3) {
				value.area.pop();
				renderValue();
			}
		});

		renderPreview();
	};

	const initialValue = () => {
		return {
			enable: false,
			area: [
				[20, 20],
				[80, 20],
				[50, 50],
			],
		};
	};

	let rendered = false;
	const id = uniqueID();

	return {
		html: `
			<li
				id="${id}"
				class="form-field"
				style="display:flex; padding-bottom:0.25rem;"
			>
				<label class="form-field-label">DOODS mask</label>
				<div style="width:auto">
					<button class="settings-edit-btn color3">
						<img src="static/icons/feather/edit-3.svg"/>
					</button>
				</div>
				${modal.html}
			</li> `,

		value() {
			return JSON.stringify(value);
		},
		set(input, _, f) {
			fields = f;
			value = input === "" ? initialValue() : JSON.parse(input);
			if (rendered) {
				renderValue();
			}
		},
		init($parent) {
			var feed;
			const element = $parent.querySelector("#" + id);
			element.querySelector(".settings-edit-btn").addEventListener("click", () => {
				if (!rendered) {
					const subInputEnabled = fields.subInput.value() !== "" ? "true" : "";
					const monitor = {
						id: fields.id.value(),
						audioEnabled: "false",
						subInputEnabled: subInputEnabled,
					};
					feed = newFeed(monitor, true, hls);

					renderModal(element, feed);
					rendered = true;
				}
				modal.open();

				feed.init($modalContent);

				modal.onClose(() => {
					feed.destroy();
				});
			});
		},
	};
}

export { doodsThresholds, doodsCrop, doodsMask };
